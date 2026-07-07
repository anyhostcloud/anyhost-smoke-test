# anyhost-smoke-test

Dockerfile-based smoke service for verifying AnyHost customer-project deployment and managed resource injection.

This app is intentionally small but resource-backed:

- `GET /health` verifies the container is running.
- `GET /ready` verifies PostgreSQL and S3 connectivity.
- `POST /artifacts` uploads a file to S3 and stores metadata in PostgreSQL.
- `GET /artifacts` lists PostgreSQL metadata.
- `GET /artifacts/{id}` reads metadata from PostgreSQL and returns the S3 object.

The container listens on port `8080`.

## AnyHost Resources

Before deploying through AnyHost, the project environment should have ready managed resources:

- Postgres resource for `DATABASE_URL`
- Storage resource for `S3_BUCKET`, `S3_PREFIX`, and `S3_REGION`. The deployed task uses its task role for S3 credentials. Legacy static key variables are also supported when provided.

After provisioning resources, refresh context:

```sh
anyhost context
```

Deploy dev:

```sh
anyhost deploy -e dev
```

## Runtime Environment

AnyHost injects these variables from ready managed resources:

```text
DATABASE_URL
S3_BUCKET
S3_PREFIX
S3_REGION
```

If any are missing, `/health` still returns `ok`, but `/ready` reports `not_ready`.

Optional legacy S3 credentials:

```text
S3_ACCESS_KEY_ID
S3_SECRET_ACCESS_KEY
```

If one legacy key is set, both must be set.

## Verify A Deployment

```sh
BASE_URL=https://anyhost-smoke-test-dev.anyhostcloud.com

curl -fsS "$BASE_URL/health"
curl -fsS "$BASE_URL/ready"

curl -fsS -F "file=@README.md;type=text/plain" "$BASE_URL/artifacts"
curl -fsS "$BASE_URL/artifacts"
```

To download an uploaded artifact, use the `id` returned from `POST /artifacts`:

```sh
curl -fsS "$BASE_URL/artifacts/<id>"
```

## Local Development

Run tests:

```sh
go test ./...
```

Run locally with real resources:

```sh
DATABASE_URL=postgres://... \
S3_BUCKET=... \
S3_PREFIX=... \
S3_REGION=... \
go run .
```

For local runs outside AWS, also set `S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY` or configure normal AWS SDK credentials.
