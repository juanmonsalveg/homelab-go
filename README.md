# homelab-go

The shared backend of the homelab apps: Go servers that run behind Traefik and Authelia on a LAN-only homelab, one container each, with the homelab's shared Postgres 18.

It gives every app the same main function and everything the homelab requires of it. That covers the login from Authelia's headers, `/healthz`, the CSRF check and security headers, the Postgres pool and migrations, serving the SPA with the right cache headers, request logs, and a shutdown that fits in Docker's stop window. An app brings its migrations, its users queries, its frontend build and its routes.

Its frontend counterpart is the npm package [`@juanchomogi/homelab-ui`](https://github.com/juanmonsalveg/homelab-ui), whose client and queries ask for what this module answers: `/healthz`, `GET /api/me`, `GET /api/users`, and errors as `{"error":{"code","message"}}`.

> Code comments cite this file's sections as `README.md §N`. Keep the numbers stable, and add new sections at the end.

## 1. What's in it

| Package           | What                                                                                                                                                                  |
| ----------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `app`             | `app.Main(app.Spec)`: the binary's subcommands (`serve`, `migrate`, `healthcheck`) and the startup sequence                                                           |
| `config`          | `config.Base`: the settings every app shares, from environment variables                                                                                              |
| `database`        | The pool (small, no TLS, retried at startup) and goose migrations from the app's `fs.FS`                                                                              |
| `database/dbtest` | A fresh schema per integration test, with the app's migrations applied                                                                                                |
| `auth`            | The logged-in user from Authelia's `Remote-*` headers, the dev fallback, `auth.UserStore`, `RequireUser`, and the `GET /api/me` and `GET /api/users` handlers         |
| `server`          | The handler around the app's routes (request log, recover, security headers, `/healthz`, auth, CSRF, the API's 404, the SPA) and the HTTP server's timeouts and shutdown |
| `httpx`           | Helpers for the app's handlers: JSON responses and errors, request bodies, 405s, log fields                                                                           |
| `health`          | `/healthz`                                                                                                                                                            |
| `spa`             | Serving the app's Vite build: the `index.html` fallback, cache headers and MIME types                                                                                 |

What stays in each app: its migrations and sqlc queries (the users table included, since each app has its own database), the `go:embed` of its migrations and its frontend, its route table and its handlers.

## 2. Using it in an app

```sh
go get github.com/juanmonsalveg/homelab-go@latest
```

An app's `cmd/<name>/main.go` is its `Spec` and its route table:

```go
func main() {
	os.Exit(app.Main(app.Spec{
		Name:       "todo",
		Migrations: db.Migrations(), // the app's own go:embed
		Web:        web.Dist(),      // nil without the embedweb tag
		Users:      users.NewPGStore,
		Routes: func(mux *http.ServeMux, d app.Deps) {
			routes(mux, lists.NewPGStore(d.Pool), d.Logger)
		},
	}))
}

// routes is the app's route table. Every route runs behind auth and CSRF.
func routes(mux *http.ServeMux, ls lists.Store, logger *slog.Logger) {
	listsAPI := lists.NewAPI(ls, logger)
	mux.HandleFunc("GET /api/lists", listsAPI.List)
	mux.HandleFunc("POST /api/lists", listsAPI.Create)
	mux.Handle("/api/lists", httpx.MethodNotAllowed("GET, HEAD, POST"))
}
```

Taking `routes`'s stores as parameters, rather than building them from `d.Pool` inside it, lets the route table be tested on fakes (§10).

- **`Spec`:**
  - `Name` is lowercase: the binary and the compose service. The usage text, error messages and the not-embedded page use it too.
  - `Migrations` is the app's `db/migrations`. `go:embed` only reaches files under the embedding package's directory, so the app embeds them itself, in `db/embed.go`:

    ```go
    //go:embed migrations/*.sql
    var migrations embed.FS

    func Migrations() fs.FS {
    	sub, err := fs.Sub(migrations, "migrations")
    	if err != nil {
    		panic(err) // fs.Sub fails only for an invalid path, and this one is constant
    	}
    	return sub
    }
    ```

  - `Web` is the app's Vite build, embedded the same way under a build tag (`//go:build embedweb` in `internal/web/embed.go`, and a `Dist()` returning nil in a `!embedweb` file). Without the tag, Go builds and tests never need a frontend build (§9).
  - `Users` builds the app's `auth.UserStore` on its pool (§5).
  - `Routes` registers the app's routes (§6). It may be nil.
  - `ContentSecurityPolicy` replaces the default policy (§6). Leave it empty.
- **`Deps`**, what `Routes` gets: the app's `*pgxpool.Pool`, the logger and the loaded `config.Base`.
- **What every app serves without registering it:** `/healthz`, `GET /api/me`, `GET /api/users`, a JSON 404 for any other `/api` path and the SPA on `/`. An app that registers one of these patterns panics at startup (`http.ServeMux` refuses a conflict).

## 3. Configuration

Everything comes from environment variables. Locally, the app's Makefile loads `.env` (there's no dotenv library). In production, compose loads it with `env_file`.

| Variable                                                 | Default      | Notes                                                                                                                                                                                                                                   |
| -------------------------------------------------------- | ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `DATABASE_URL`                                           | required     | A `postgres://` URL. Without an `sslmode` parameter, `sslmode=disable` is added (the homelab Postgres has no TLS), so the server's `.env` doesn't need editing. Never logged, and never quoted in an error, since it holds the password. |
| `APP_URL`                                                | required     | The app's origin, for the CSRF check and any absolute URL. It must be a bare origin (no path or query) and is normalized to the form browsers send in `Origin`. Production: `https://<app>.lab.juanmonsalve.dev`. Dev: `http://localhost:5173`, the Vite origin the browser actually sends. |
| `PORT`                                                   | `3000`       | The app listens on `0.0.0.0:$PORT`                                                                                                                                                                                                      |
| `APP_ENV`                                                | `production` | Only the exact value `development` turns on dev behaviour. Anything other than empty, `production` or `development` is a startup error, so a typo never runs as either mode. The Dockerfile sets `production`.                          |
| `DEV_REMOTE_USER`, `DEV_REMOTE_NAME`, `DEV_REMOTE_EMAIL` | empty        | Stand-ins for Authelia's headers, used only when `APP_ENV=development` and the request has no `Remote-User`. Startup logs a warning while the fallback is active.                                                                         |
| `DB_MAX_CONNS`                                           | `5`          | From 1 to 10. The server's 100 connections are shared by every app.                                                                                                                                                                     |
| `MIGRATE_ON_START`                                       | `true`       | `false` runs migrations only by hand (`<app> migrate`)                                                                                                                                                                                  |
| `LOG_LEVEL`                                              | `info`       | `debug`, `info`, `warn` or `error`                                                                                                                                                                                                      |

If a variable is missing or invalid, the app prints every problem, one per line, and exits 1. It never starts half-configured. The server's `.env` has `DATABASE_URL` and `APP_URL`, and `APP_ENV` and `DEV_REMOTE_*` stay unset there.

An app with settings of its own reads them next to `config.Base`, from the same environment.

## 4. Database and migrations

- **The pool** (`database.PoolConfig`, `database.Connect`): `MaxConns = DB_MAX_CONNS` (hard cap 10, enforced here too), `MinConns = 0`, `MaxConnIdleTime = 5m`, and `sslmode=disable` unless the URL says otherwise.
- **The first connection is retried**, with delays from 0.5 s doubling up to 5 s, for about 60 s in total. After the server's 04:00 reboot an app can start before Postgres, and `depends_on` can't help, because Postgres lives in another compose project. If the retries run out, the app exits 1 and `restart: unless-stopped` brings it back. A signal while it waits is a clean stop, not a failure.
- **Migrations** are goose v3, used as a library inside the binary, on the app's `fs.FS` (`database.Migrate`, `database.MigrationStatus`). Why goose:
  - Each SQL migration runs in a transaction. Postgres DDL is transactional, so a failed migration leaves nothing half-applied and re-runs cleanly on the next start. golang-migrate, by contrast, marks the database "dirty" and needs manual repair.
  - Up-only files are first-class, which fits forward-only migrations.
  - A Postgres advisory lock (goose's session locker) keeps a run at startup and a manual `migrate` from racing. A run queued behind another checks every second, for up to 5 minutes.
  - sqlc reads goose files directly as its schema.
  - It ships inside the binary, so `docker compose run --rm <app> migrate status` works on the server as the app's role, with nothing else to install.
- **They run as the role in `DATABASE_URL`**, the app's own, so the tables it creates are owned by that role. goose needs a `database/sql` handle, so each run opens one with `pgx/v5/stdlib` on a single connection and closes it afterwards, which also releases the lock.
- **The startup sequence** (`<app> serve`):
  1. Load and validate the config.
  2. Connect the pool, with the retries above.
  3. If `MIGRATE_ON_START` is set, apply the pending migrations under the lock. Exit 1 on failure.
  4. Start HTTP.

How to write the migrations themselves (file names, Up only, never editing one that's on `main`, backward compatibility) is each app's own convention, in its ARCHITECTURE.md §6.

## 5. Login and the people

The app has no login of its own. Authelia logs people in, through Traefik's forward-auth, and adds `Remote-User`, `Remote-Name` and `Remote-Email` to every request it lets through. Traefik replaces any `Remote-*` header a browser sends and the app publishes no ports, so the headers can be trusted. That holds only while the app's router keeps its `authelia@docker` middleware label.

- **`auth.Middleware`**, on every route but `/healthz`:
  - Reads the three headers, trimmed. A `Remote-User` of only whitespace counts as missing.
  - If `Remote-User` is missing and the config has a dev user (only with `APP_ENV=development`), uses the `DEV_REMOTE_*` values.
  - If there's still no user, answers `401 {"error":{"code":"unauthenticated"}}`.
  - Otherwise upserts the user through the app's `UserStore` and puts the row (`auth.User{ID, Username, DisplayName, Email}`) in the request context. It also adds `user` to the request's log line.
  - The upsert is the one write a GET may cause, which the homelab requires, so the users table always has everyone who has opened the app, with Authelia's current name and email. One upsert per request is fine for a household. If it ever shows up in the logs, cache the username's id in-process and skip the write when nothing changed in the last 5 minutes.
- **In a handler**, `auth.RequireUser(w, r, logger)` returns the user. Without one, the handler was mounted outside the middleware (a wiring bug), so it logs, answers 500 and returns false, and the handler returns.
- **`GET /api/me`** (`auth.MeHandler`): `200 {"id","username","displayName"}`, the caller. The shell shows the name, and the UI uses the id wherever it needs to know which person is the caller. No email.
- **`GET /api/users`** (`auth.UsersHandler`): `200 {"users":[{"id","username","displayName"}]}`, everyone who has opened the app except the caller, by display name. In an app the household shares, these are the people something can be shared with or belong to. No email.
- **`auth.UserStore`** is the app's, because the users table is: each app has its own database, with its own copy of the migration and queries. It has two methods, `UpsertUser(ctx, auth.Identity) (auth.User, error)` and `OtherUsers(ctx, userID) ([]auth.Person, error)`. The table and the queries behind them:

  ```sql
  -- db/migrations/00001_create_users.sql
  -- +goose Up
  CREATE TABLE users (
      id           uuid        PRIMARY KEY DEFAULT uuidv7(),
      username     text        NOT NULL UNIQUE,
      display_name text        NOT NULL DEFAULT '',
      email        text        NOT NULL DEFAULT '',
      created_at   timestamptz NOT NULL DEFAULT now(),
      last_seen_at timestamptz NOT NULL DEFAULT now()
  );
  ```

  ```sql
  -- db/queries/users.sql
  -- name: UpsertUser :one
  -- One statement, so two first requests for the same username can't collide.
  INSERT INTO users (username, display_name, email)
  VALUES ($1, $2, $3)
  ON CONFLICT (username) DO UPDATE
     SET display_name = EXCLUDED.display_name,
         email        = EXCLUDED.email,
         last_seen_at = now()
  RETURNING id, username, display_name, email;

  -- name: ListOtherUsers :many
  SELECT id, username, display_name
  FROM users
  WHERE id <> @user_id
  ORDER BY display_name, username;
  ```

  The app's `internal/users` package adapts the sqlc code to the interface: about 50 lines, with `NewPGStore(pool) auth.UserStore` so it can be `Spec.Users` as it is. Users are keyed on the Authelia username, so renaming an Authelia account makes a new, empty user.
- `Remote-Groups` isn't read yet. Today any Authelia account can open an app, and restricting one to a group is done in Authelia's access rules.

## 6. The handler

`server.NewHandler` wraps the app's routes in this order, outermost first:

1. The request log: one line per request, with the method, path, status, duration, the client IP (the last `X-Forwarded-For` entry, which Traefik appends) and any fields handlers add, the username among them.
2. Recover: a panic becomes a logged error and a JSON 500, inside the request log, so the request line still shows the 500.
3. Security headers.
4. `/healthz`, before auth.
5. Auth (§5).
6. CSRF.
7. The routes: the app's, plus the module's.

- **`/healthz`** is mounted on the top-level mux, outside auth, so it can never require a login by accident. `GET` (and so `HEAD`, which `curl -I` sends) answers `200 {"status":"ok"}` when `SELECT 1` succeeds within 2 s, otherwise `503 {"status":"unavailable"}`, with `Cache-Control: no-store`. It never reads `Remote-*` headers, and error details go only to the logs, since the endpoint is open. Other methods get a 405. Authelia lets `/healthz` through with a bypass rule, Uptime Kuma and the container healthcheck call it, and Traefik routes to the app only while it answers 200, so a database outage shows up as a 404 at the app's URL.
- **CSRF:** Authelia's login cookie is set for `lab.juanmonsalve.dev`, so the browser also sends it when another page, another `*.lab` site included, makes a request to the app. So every method except `GET`, `HEAD` and `OPTIONS` must carry an `Origin` equal to `APP_URL`. Otherwise, including when `Origin` is missing (browsers always send one with these methods), it answers `403 {"error":{"code":"cross_origin"}}` and logs the rejected origin on the request line, so a wrong `APP_URL` shows up as every write failing. Go's `http.CrossOriginProtection` isn't layered on top: with `Origin` required and matched exactly, it adds nothing. Safe methods pass unchecked, so **GET handlers must never change the app's data**.
- **Security headers**, on every response, `/healthz` and errors included:
  - `X-Content-Type-Options: nosniff`
  - `Referrer-Policy: same-origin`
  - `Content-Security-Policy: default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'` (`server.DefaultContentSecurityPolicy`)

  `style-src` allows inline styles for the `<style>` tags Mantine renders at runtime. Scripts have no directive of their own, so they stay at `default-src 'self'`, and a test checks that `'unsafe-inline'` appears only in `style-src`. `frame-ancestors 'none'` also blocks clickjacking from other `*.lab` sites. An app replaces the policy with `Spec.ContentSecurityPolicy` only when something actually breaks.
- **The routes the module adds:** `GET /api/me` and `GET /api/users` (§5), each with a JSON 405 and `Allow: GET, HEAD` for other methods; a JSON `404 {"error":{"code":"not_found","message":"No such API route"}}` for any other path under `/api` (and `/api` itself, which `ServeMux` would otherwise redirect), so an API typo never gets the SPA's HTML; and the SPA on `/` (§9), which needs a login like everything else.
- **The app's routes** use method patterns (`GET /api/lists`, `PATCH /api/lists/{id}`), which also match `HEAD` for a `GET`. Mount `httpx.MethodNotAllowed("GET, HEAD, POST")` on the same path without a method, so other methods get the API's error shape and an `Allow` header instead of net/http's plain-text 405.
- **Behind the proxy:** never build a URL from `r.Host` (use `APP_URL`), use `X-Forwarded-*` headers only for logging, and set no cookies. If an app ever does, they must be `Secure; HttpOnly; SameSite=Lax`.

## 7. HTTP helpers

`httpx` is for the app's handlers as much as for this module's.

- **Responses:** `httpx.WriteJSON(w, status, v)` and `httpx.WriteError(w, status, code, message)`, which writes the API's error shape, `{"error":{"code":"...","message":"..."}}`. The UI picks its words from `code`, never shows `message`, so codes are part of the API and messages are for people reading logs.
- **Request bodies:** `httpx.DecodeJSON(w, r, &req)` reads exactly one JSON value, with no fields the struct lacks, of at most 1 MB (`httpx.MaxBodyBytes`). A bad body is `400 invalid_json`, an oversized one `413 body_too_large`, and it returns false so the handler returns.
  - A field that must be present but may be `null`, like a move's `afterId`, is an `httpx.NullableID`, so a missing field is a 400 rather than silently meaning `null`.
  - A field that may be left out but never `null`, like a `PATCH`'s fields, is an `httpx.Optional[T]`: leaving it out keeps the value, so `null` would be ambiguous and is a 400.
- **405s:** `httpx.MethodNotAllowed(allow)` (§6).
- **The log line:** `httpx.AddLogAttrs(r.Context(), slog.String(...))` adds fields to the current request's line. `httpx.LogRequests` and `httpx.Recover` are the middleware `server` uses.
- **JSON conventions:** camelCase fields and RFC 3339 UTC timestamps. The frontend converts times to local time only for display.

## 8. Server and process

- **The binary's commands** (`app.Main`):
  - `serve`, the default: the startup sequence (§4), then HTTP until `SIGTERM` or `SIGINT`.
  - `migrate [up|status]`: apply the pending migrations, or list every migration with when it was applied. On the server: `docker compose run --rm <app> migrate status`.
  - `healthcheck`: `GET http://127.0.0.1:$PORT/healthz` with a 3 s timeout, exiting 0 on a 200 and 1 otherwise, redirects included. The runtime image is distroless, with no curl or wget, so the binary checks itself for the compose healthcheck. It reads only `PORT`, so a broken configuration still reports through `/healthz`.
  - `help`. Exit codes are 0 on success, 1 on failure and 2 for a usage error.
- **Listening:** plain HTTP on `0.0.0.0:$PORT`. Traefik handles TLS, and binding `127.0.0.1` instead would make it answer 502.
- **Timeouts:** `ReadHeaderTimeout 5s`, `ReadTimeout 15s`, `WriteTimeout 30s`, `IdleTimeout 120s`.
- **Shutdown:** on `SIGTERM` (`docker stop`) or `SIGINT` (Ctrl-C), stop accepting connections and wait for in-flight requests for at most 8 s, then close the pool. Docker kills at 10 s. Requests still running after 8 s have their connections closed. Once the signal arrives the default handlers come back, so a second Ctrl-C exits at once.
- **Logging:** stdout and stderr only. JSON in production, read with `docker compose logs`, and text with `APP_ENV=development`. Startup logs the app's name, mode and `APP_URL`, and a warning while the dev login fallback is active.

## 9. Serving the SPA

`spa.Handler(name, files, logger)`, mounted on `/` by `server`, serves `GET` and `HEAD` from the app's Vite build:

- A path that isn't a file gets `index.html`, so the SPA can route it, except under `/assets/`: a missing hashed file is a plain 404, never HTML that would be cached as a script for a year.
- Cache headers: files under `/assets/` have hashed names and get `public, max-age=31536000, immutable`. Everything else, `index.html`, `sw.js` and the manifest included, gets `no-cache`, so a deploy reaches the browser and the service worker at once.
- `.webmanifest` is served as `application/manifest+json` and `.ico` as `image/x-icon`. Go's built-in MIME table has neither, and the distroless image has no `mime.types`.
- With `files` nil (a binary built without the frontend), every page is a short notice titled with the app's name, pointing to `make dev` and `make run`.
- Other methods get a JSON 405.

Which files the service worker precaches and caches is the frontend's business, in `@juanchomogi/homelab-ui/pwa`. This handler only makes sure the browser revalidates what it must.

## 10. Testing an app

- **Handlers** take small interfaces (a `lists.Store`, say), so their tests use fakes and need no database. To stand in for a logged-in user, put one in the request's context with `auth.NewContext(r.Context(), auth.User{...})`.
- **The route table:** build the real handler with `server.NewHandler(server.Deps{...}, routes)`, with a fake `health.DB` (one `QueryRow` method), a fake `auth.UserStore` and the app's `routes` on fake stores. Then check each route's status and `Allow`, and that each write route answers 403 without the right `Origin`. This module tests the chain itself, `/healthz`, `/api/me` and `/api/users`, so an app tests only its own routes.
- **Integration tests** use `TEST_DATABASE_URL`, an app's local `<app>_test` database, and are skipped when it isn't set, so `make test` stays free of databases.
  - `dbtest.Pool(t, db.Migrations())` returns a pool on a new schema (set through `search_path`) with every migration applied. The schema is dropped when the test ends, so tests start from zero and can run in parallel.
  - `dbtest.URL(t)` returns just the URL of a new, empty schema, for a test that runs the migrations itself.
- Test fixtures use made-up names, never the household's.

## 11. Working on this module

- **Setup:** `mise install`, then `make setup`.
- **`make check`** runs `go mod tidy -diff`, `go vet`, golangci-lint (formatting included) and the unit tests. Run it before calling a change done.
- **Integration tests:** `make db-up` starts a throwaway Postgres 18 on `127.0.0.1:5433` (`compose.dev.yml`), next to an app's own dev database on 5432. Then `make test-integration`, and `make db-down` afterwards. CI runs `make check` and the integration tests against a `postgres:18` service on every push and pull request.
- **Trying a change in an app before releasing:** put a `go.work` next to the two checkouts, outside both repos:

  ```sh
  cd ~/code/personal
  go work init ./todo ./homelab-go
  ```

  The app then builds and tests against the local module, and there's no `replace` directive to forget. This needs the version the app requires to exist, since Go still reads its `go.mod`, so it's for an app already on a release. Before committing in the app, run its `make check` with `GOWORK=off`, which is what CI and the server build see. Delete the `go.work` afterwards.
- **Versions** are `v0.x.y`. Bump the minor for a breaking change and the patch otherwise. Dependabot opens a PR in each app for every new version, minor ones included, so a breaking change shows up as an app's CI failing on that PR, where it's fixed before merging.
- **The module's own dependencies** are what every app gets at least, so keep their versions at what the apps can take. goose especially: an app's `make lint` fails until the goose CLI in its `mise.toml` matches the goose module it builds with.
- **Releasing:** once CI passes on `main`, `git tag v0.2.0 && git push origin v0.2.0`. That's all a Go module needs: the Go proxy fetches the tag the first time someone asks for it, and `GOPROXY=https://proxy.golang.org go list -m github.com/juanmonsalveg/homelab-go@v0.2.0` asks right away. The proxy and the checksum database keep a version forever, so **never move or reuse a tag**. A broken release is fixed by the next patch, and marked with a `retract` directive in `go.mod` if apps must not pick it.
- **Code comments** cite this file's sections as `README.md §N`.
- **License:** MIT.
