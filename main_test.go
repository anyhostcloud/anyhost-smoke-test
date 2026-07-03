package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestArtifactUploadStoresObjectAndMetadata(t *testing.T) {
	metadata := newFakeArtifactStore()
	objects := newFakeObjectStore()
	handler := newApp(metadata, objects)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="hello.txt"`)
	header.Set("Content-Type", "text/plain; charset=utf-8")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("hello anyhost")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/artifacts", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /artifacts status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created artifactRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Name != "hello.txt" || created.SizeBytes != int64(len("hello anyhost")) {
		t.Fatalf("unexpected artifact record: %#v", created)
	}
	if created.SHA256 != "9706d9623c03a6c5fbd0787ba06caf576ffecfa1eb1fa3a20235439389da274e" {
		t.Fatalf("sha256 = %q", created.SHA256)
	}
	if len(metadata.records) != 1 {
		t.Fatalf("metadata records = %d, want 1", len(metadata.records))
	}
	if got := string(objects.objects[created.S3Key].body); got != "hello anyhost" {
		t.Fatalf("stored object body = %q", got)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/artifacts", nil)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /artifacts status = %d, body = %s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Artifacts []artifactRecord `json:"artifacts"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Artifacts) != 1 || list.Artifacts[0].ID != created.ID {
		t.Fatalf("unexpected artifact list: %#v", list.Artifacts)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/artifacts/"+created.ID, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /artifacts/{id} status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	if getRec.Body.String() != "hello anyhost" {
		t.Fatalf("download body = %q", getRec.Body.String())
	}
	if contentType := getRec.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Fatalf("download content-type = %q", contentType)
	}
}

func TestReadyChecksPostgresAndStorage(t *testing.T) {
	handler := newApp(newFakeArtifactStore(), newFakeObjectStore())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy ready status = %d, body = %s", rec.Code, rec.Body.String())
	}

	metadata := newFakeArtifactStore()
	metadata.pingErr = errors.New("postgres down")
	rec = httptest.NewRecorder()
	newApp(metadata, newFakeObjectStore()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy ready status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestS3ObjectStoreListPrefixMatchesAnyHostPolicy(t *testing.T) {
	store := &s3ObjectStore{prefix: "workspaces/wsp/projects/prj/envs/dev/storage/uploads"}
	if got := store.listPrefix(); got != "workspaces/wsp/projects/prj/envs/dev/storage/uploads/" {
		t.Fatalf("listPrefix() = %q", got)
	}
	if got := store.key("artifact.txt"); got != "workspaces/wsp/projects/prj/envs/dev/storage/uploads/artifact.txt" {
		t.Fatalf("key() = %q", got)
	}

	store = &s3ObjectStore{}
	if got := store.listPrefix(); got != "" {
		t.Fatalf("empty listPrefix() = %q", got)
	}
	if got := store.key("/artifact.txt"); got != "artifact.txt" {
		t.Fatalf("empty-prefix key() = %q", got)
	}
}

func TestS3PutObjectInputUsesContentLengthAndScopedKey(t *testing.T) {
	store := &s3ObjectStore{
		bucket: "bucket",
		prefix: "workspaces/wsp/projects/prj/envs/dev/storage/uploads",
	}
	input := store.putObjectInput("/artifact.txt", []byte("hello"), "text/plain")

	if input.Bucket == nil || *input.Bucket != "bucket" {
		t.Fatalf("Bucket = %#v", input.Bucket)
	}
	if input.Key == nil || *input.Key != "workspaces/wsp/projects/prj/envs/dev/storage/uploads/artifact.txt" {
		t.Fatalf("Key = %#v", input.Key)
	}
	if input.ContentLength == nil || *input.ContentLength != 5 {
		t.Fatalf("ContentLength = %#v", input.ContentLength)
	}
	if input.ContentType == nil || *input.ContentType != "text/plain" {
		t.Fatalf("ContentType = %#v", input.ContentType)
	}
}

func TestS3ClientPutObjectReachesEndpointWithoutExpectContinue(t *testing.T) {
	var gotPath string
	var gotExpect string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotExpect = r.Header.Get("Expect")
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  server.Client(),
	}
	store := &s3ObjectStore{
		client: newS3Client(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(server.URL)
			o.UsePathStyle = true
		}),
		bucket: "bucket",
	}
	body := bytes.Repeat([]byte("a"), 3<<20)

	if err := store.put(context.Background(), "artifact.txt", body, "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if gotPath != "/bucket/artifact.txt" {
		t.Fatalf("request path = %q, want /bucket/artifact.txt", gotPath)
	}
	if gotExpect != "" {
		t.Fatalf("Expect header = %q, want empty", gotExpect)
	}
}

type fakeArtifactStore struct {
	records map[string]artifactRecord
	pingErr error
}

func newFakeArtifactStore() *fakeArtifactStore {
	return &fakeArtifactStore{records: map[string]artifactRecord{}}
}

func (s *fakeArtifactStore) create(ctx context.Context, record artifactRecord) error {
	s.records[record.ID] = record
	return nil
}

func (s *fakeArtifactStore) list(ctx context.Context) ([]artifactRecord, error) {
	records := make([]artifactRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	return records, nil
}

func (s *fakeArtifactStore) get(ctx context.Context, id string) (artifactRecord, error) {
	record, ok := s.records[id]
	if !ok {
		return artifactRecord{}, errNotFound
	}
	return record, nil
}

func (s *fakeArtifactStore) ping(ctx context.Context) error {
	return s.pingErr
}

type fakeObjectStore struct {
	objects map[string]fakeObject
	pingErr error
}

type fakeObject struct {
	body        []byte
	contentType string
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string]fakeObject{}}
}

func (s *fakeObjectStore) put(ctx context.Context, key string, body []byte, contentType string) error {
	s.objects[key] = fakeObject{body: body, contentType: contentType}
	return nil
}

func (s *fakeObjectStore) get(ctx context.Context, key string) ([]byte, string, error) {
	object, ok := s.objects[key]
	if !ok {
		return nil, "", errNotFound
	}
	return object.body, object.contentType, nil
}

func (s *fakeObjectStore) ping(ctx context.Context) error {
	return s.pingErr
}
