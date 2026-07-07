package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/lib/pq"
)

const maxArtifactBytes = 16 << 20

var errNotFound = errors.New("not found")

type artifactRecord struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	SHA256      string    `json:"sha256"`
	S3Key       string    `json:"s3_key"`
	CreatedAt   time.Time `json:"created_at"`
}

type artifactStore interface {
	create(context.Context, artifactRecord) error
	list(context.Context) ([]artifactRecord, error)
	get(context.Context, string) (artifactRecord, error)
	ping(context.Context) error
}

type objectStore interface {
	put(context.Context, string, []byte, string) error
	get(context.Context, string) ([]byte, string, error)
	ping(context.Context) error
}

type app struct {
	artifacts artifactStore
	objects   objectStore
}

func main() {
	ctx := context.Background()
	handler := newApp(storeFromEnv(ctx), objectStoreFromEnv(ctx))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}

func newApp(artifacts artifactStore, objects objectStore) http.Handler {
	a := &app{artifacts: artifacts, objects: objects}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /ready", a.handleReady)
	mux.HandleFunc("POST /artifacts", a.handleCreateArtifact)
	mux.HandleFunc("GET /artifacts", a.handleListArtifacts)
	mux.HandleFunc("GET /artifacts/{id}", a.handleGetArtifact)
	mux.HandleFunc("GET /", a.handleRoot)
	return mux
}

func (a *app) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("anyhost smoke test is running\n"))
}

func (a *app) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"service": "anyhost-smoke-test",
		"status":  "ok",
	})
}

func (a *app) handleReady(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	status := http.StatusOK
	if err := a.artifacts.ping(r.Context()); err != nil {
		checks["postgres"] = err.Error()
		status = http.StatusServiceUnavailable
	} else {
		checks["postgres"] = "ok"
	}
	if err := a.objects.ping(r.Context()); err != nil {
		checks["storage"] = err.Error()
		status = http.StatusServiceUnavailable
	} else {
		checks["storage"] = "ok"
	}
	state := "ready"
	if status != http.StatusOK {
		state = "not_ready"
	}
	writeJSON(w, status, map[string]any{
		"service": "anyhost-smoke-test",
		"status":  state,
		"checks":  checks,
	})
}

func (a *app) handleCreateArtifact(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBytes)
	if err := r.ParseMultipartForm(maxArtifactBytes); err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart form file")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()

	body, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read file")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "file is empty")
		return
	}

	id, err := newID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create artifact id")
		return
	}
	name := safeFilename(header.Filename)
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	hash := sha256.Sum256(body)
	record := artifactRecord{
		ID:          id,
		Name:        name,
		ContentType: contentType,
		SizeBytes:   int64(len(body)),
		SHA256:      hex.EncodeToString(hash[:]),
		S3Key:       "artifacts/" + id + "/" + name,
		CreatedAt:   time.Now().UTC(),
	}
	if err := a.objects.put(r.Context(), record.S3Key, body, record.ContentType); err != nil {
		writeDetailedError(w, http.StatusInternalServerError, "could not store artifact object", err)
		return
	}
	if err := a.artifacts.create(r.Context(), record); err != nil {
		writeDetailedError(w, http.StatusInternalServerError, "could not store artifact metadata", err)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (a *app) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	records, err := a.artifacts.list(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list artifacts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": records})
}

func (a *app) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	record, err := a.artifacts.get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusNotFound, "artifact not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not load artifact metadata")
		return
	}
	body, contentType, err := a.objects.get(r.Context(), record.S3Key)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusNotFound, "artifact object not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not load artifact object")
		return
	}
	if contentType == "" {
		contentType = record.ContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, record.Name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

type postgresArtifactStore struct {
	db *sql.DB
}

func storeFromEnv(ctx context.Context) artifactStore {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		return missingArtifactStore("DATABASE_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return missingArtifactStore("open postgres: " + err.Error())
	}
	return &postgresArtifactStore{db: db}
}

func (s *postgresArtifactStore) ensureSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS artifacts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			content_type TEXT NOT NULL,
			size_bytes BIGINT NOT NULL,
			sha256 TEXT NOT NULL,
			s3_key TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)
	`)
	return err
}

func (s *postgresArtifactStore) create(ctx context.Context, record artifactRecord) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artifacts (id, name, content_type, size_bytes, sha256, s3_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, record.ID, record.Name, record.ContentType, record.SizeBytes, record.SHA256, record.S3Key, record.CreatedAt)
	return err
}

func (s *postgresArtifactStore) list(ctx context.Context) ([]artifactRecord, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, content_type, size_bytes, sha256, s3_key, created_at
		FROM artifacts
		ORDER BY created_at DESC
		LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []artifactRecord
	for rows.Next() {
		var record artifactRecord
		if err := rows.Scan(&record.ID, &record.Name, &record.ContentType, &record.SizeBytes, &record.SHA256, &record.S3Key, &record.CreatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *postgresArtifactStore) get(ctx context.Context, id string) (artifactRecord, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return artifactRecord{}, err
	}
	var record artifactRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, content_type, size_bytes, sha256, s3_key, created_at
		FROM artifacts
		WHERE id = $1
	`, id).Scan(&record.ID, &record.Name, &record.ContentType, &record.SizeBytes, &record.SHA256, &record.S3Key, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return artifactRecord{}, errNotFound
	}
	return record, err
}

func (s *postgresArtifactStore) ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	return s.ensureSchema(ctx)
}

type s3ObjectStore struct {
	client *s3.Client
	bucket string
	prefix string
}

func objectStoreFromEnv(ctx context.Context) objectStore {
	bucket := strings.TrimSpace(os.Getenv("S3_BUCKET"))
	region := strings.TrimSpace(os.Getenv("S3_REGION"))
	if bucket == "" || region == "" {
		return missingObjectStore("S3_BUCKET and S3_REGION are required")
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	accessKey := strings.TrimSpace(os.Getenv("S3_ACCESS_KEY_ID"))
	secretKey := strings.TrimSpace(os.Getenv("S3_SECRET_ACCESS_KEY"))
	if accessKey != "" || secretKey != "" {
		if accessKey == "" || secretKey == "" {
			return missingObjectStore("S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY must be provided together")
		}
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return missingObjectStore("load aws config: " + err.Error())
	}
	cfg.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	return &s3ObjectStore{
		client: newS3Client(cfg),
		bucket: bucket,
		prefix: strings.Trim(os.Getenv("S3_PREFIX"), "/"),
	}
}

func newS3Client(cfg aws.Config, optFns ...func(*s3.Options)) *s3.Client {
	options := []func(*s3.Options){
		func(o *s3.Options) {
			o.ContinueHeaderThresholdBytes = -1
		},
	}
	options = append(options, optFns...)
	return s3.NewFromConfig(cfg, options...)
}

func (s *s3ObjectStore) put(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.putObjectInput(key, body, contentType))
	return err
}

func (s *s3ObjectStore) putObjectInput(key string, body []byte, contentType string) *s3.PutObjectInput {
	size := int64(len(body))
	return &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.key(key)),
		Body:          bytes.NewReader(body),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	}
}

func (s *s3ObjectStore) get(ctx context.Context, key string) ([]byte, string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		return nil, "", err
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	return body, aws.ToString(out.ContentType), nil
}

func (s *s3ObjectStore) ping(ctx context.Context) error {
	_, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(s.listPrefix()),
		MaxKeys: aws.Int32(1),
	})
	return err
}

func (s *s3ObjectStore) listPrefix() string {
	if s.prefix == "" {
		return ""
	}
	return s.prefix + "/"
}

func (s *s3ObjectStore) key(key string) string {
	key = strings.TrimLeft(key, "/")
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

type missingArtifactStore string

func (s missingArtifactStore) create(context.Context, artifactRecord) error {
	return errors.New(string(s))
}

func (s missingArtifactStore) list(context.Context) ([]artifactRecord, error) {
	return nil, errors.New(string(s))
}

func (s missingArtifactStore) get(context.Context, string) (artifactRecord, error) {
	return artifactRecord{}, errors.New(string(s))
}

func (s missingArtifactStore) ping(context.Context) error {
	return errors.New(string(s))
}

type missingObjectStore string

func (s missingObjectStore) put(context.Context, string, []byte, string) error {
	return errors.New(string(s))
}

func (s missingObjectStore) get(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New(string(s))
}

func (s missingObjectStore) ping(context.Context) error {
	return errors.New(string(s))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeDetailedError(w http.ResponseWriter, status int, message string, err error) {
	writeJSON(w, status, map[string]string{
		"error":  message,
		"detail": err.Error(),
	})
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func safeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "artifact.bin"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	clean := strings.Trim(b.String(), "._-")
	if clean == "" {
		return "artifact.bin"
	}
	return clean
}
