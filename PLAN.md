# Wako: Markdown Wiki — Phased Implementation Plan

Requirements are modeled on LeafWiki (single binary, page tree, Markdown editing, full-text search, image paste, roles, base-path, public read-only mode). Differences from LeafWiki:

- **Storage:** SQLite instead of Markdown files on disk plus `tree.json`. A small **catalog database** (`wako.db`) holds the space list, users, memberships and sessions. **Each space has its own database** (`spaces/<id>.db`) holding its pages, revisions, images, links and search index. Writes are atomic, and a space can be backed up, moved or deleted as one file.
- **Spaces:** one server hosts many independent wikis ("spaces"), each with its own authorization.
- **Versioning:** page revision history is included from the start. It is LeafWiki's biggest stated gap.

## 1. Decisions

| Topic | Decision |
|---|---|
| Language | Go 1.27 (`net/http` `ServeMux` method and path patterns) |
| Executables | `wakod` (server) and `wako` (CLI client). Each is a static binary with no runtime dependencies. |
| SQLite driver | `modernc.org/sqlite` (pure Go, no CGO), so `CGO_ENABLED=0` cross-compiles to linux/darwin/windows on amd64/arm64. FTS5 is built in. |
| Markdown to HTML | `goldmark` with the GitHub Flavored Markdown extensions only (tables, strikethrough, task lists, autolinks; changed from strict CommonMark in Phase 9): no footnotes, and raw HTML is omitted. A task list box is drawn as an emoji (✅ / ⬜) so no `<input>` is ever emitted; the markdown content type says `variant=GFM`. The output is still sanitized with `bluemonday`. |
| UI | Plain HTML/CSS/JS via `go:embed`, no Node build. Textarea editor with server-rendered live preview. Mermaid is vendored. |
| Content negotiation | `Accept: text/markdown` returns raw Markdown. A browser (`text/html`) gets rendered HTML. `application/json` returns metadata. |
| `.md` alternative | A `.md` suffix on any page URL forces Markdown, regardless of `Accept`. It also works on `PUT`, `POST` and `DELETE`. Page slugs therefore may not end in `.md`. |
| Spaces | Every page and asset belongs to exactly one space. URLs are `/{space}/{page-path}`. |
| Anonymous access | Anonymous visitors get read-only access. Each space has a `public_read` flag, default **true**. An admin can switch it off for a private space. Anonymous users can never write. |
| Asset URLs | Space-scoped: `/{space}/_assets/{id}/{name}` |

## 2. URL and API layout

Reserved first path segments: `_api`, `_ui`, `healthz`. These cannot be space slugs. Inside a space, any page slug starting with `_` is reserved (`_assets`, `_api`).

### Pages (`/{space}/{path}`)

| Method | Path | Behavior |
|---|---|---|
| GET | `/{space}/{path}` or `/{space}/{path}.md` | Page. `Accept` picks the representation, `.md` forces Markdown. `?rev=N` returns an old revision. Responses carry an `ETag`. |
| PUT | `/{space}/{path}[.md]` | Create or replace a page. Body is `text/markdown`. `If-Match: <etag>` gives optimistic locking (412 on conflict). 201 if created, 200 if updated. |
| POST | `/{space}/{path}` | Create a child page under `{path}`. Title comes from JSON or a `Slug:` header. Also used for actions: `?move=<newpath>` and `?reorder`. |
| DELETE | `/{space}/{path}` | Delete a page. Returns 409 if it has children, unless `?recursive=true`. |
| GET | `/{space}/` | Space home page (the root page, or the tree if none is set) |

`HEAD` and `OPTIONS` are supported on every route. See section 2a for the headers that signal editability.

### Per-space API (`/_api/spaces/{space}/…`)

| Method | Path | Behavior |
|---|---|---|
| GET | `/tree` | Page tree as JSON |
| GET | `/search?q=` | Full-text search with snippets within the space |
| GET | `/pages/{path}/revisions`, `/backlinks` | History and backlinks |
| POST | `/render` | Render Markdown for the editor preview |
| POST | `/import`, GET `/export` | Zip import and export of one space |
| GET/PUT/DELETE | `/members[/{user}]` | Space membership (space admin) |

### Assets (`/{space}/_assets/…`)

| Method | Path | Behavior |
|---|---|---|
| POST | `/{space}/_assets` | Upload an image (multipart or raw body). Returns `{id, url}`. Needs editor role. |
| GET | `/{space}/_assets/{id}/{name}` | Serve with `Content-Type`, `ETag` and immutable caching. Same read rules as the space's pages. |
| DELETE | `/{space}/_assets/{id}` | Delete an image (editor) |

### Global API (`/_api/…`)

| Method | Path | Behavior |
|---|---|---|
| GET | `/spaces` | Spaces visible to the caller |
| POST/PUT/DELETE | `/spaces[/{space}]` | Create, update (title, `public_read`) or delete a space (server admin) |
| POST | `/login`, `/logout`, `/refresh` | Session auth |
| GET | `/search?q=` | Optional search across all spaces the caller can read |
| GET/POST/PUT/DELETE | `/users[/{id}]` | User management (server admin) |

`GET /` lists the spaces the caller can read.

## 2a. Signaling editability (HTTP headers)

Every response on a page URL (GET, HEAD, OPTIONS, and 405) carries standard headers, so any generic HTTP client can discover what it may do with a single `HEAD` or `OPTIONS`.

**Page (document) response:**
```
HTTP/1.1 200 OK
Content-Type: text/markdown; charset=utf-8; variant=CommonMark
ETag: "v7"
Allow: GET, HEAD, PUT, PATCH, DELETE, OPTIONS
Accept-Patch: text/x-diff
Link: </main/docs/foo.md>; rel="edit"
Link: </main/docs/foo?edit>; rel="edit-form"
WAC-Allow: user="read write", public="read"
Vary: Accept, Authorization, Cookie
```

**Page that has children, or a space root (collection):**
```
Allow: GET, HEAD, POST, PUT, DELETE, OPTIONS
Accept-Post: text/markdown, application/json
Link: </main/docs/new?create>; rel="create-form"
Link: <http://www.w3.org/ns/ldp#BasicContainer>; rel="type"
```
A page is a document and a container at once. Pages with children include both sets: `Accept-Patch`, `Accept-Post` and the `ldp:BasicContainer` type.

Rules:
- **`Allow` is filtered by the caller's effective access** (section 3), not just by what the resource supports. An anonymous visitor to a public space sees `Allow: GET, HEAD, OPTIONS`. An editor sees the full list. Advertising `PUT` and then answering 403 would be noise, and we already know the answer.
- **`WAC-Allow`** states the same thing in an explicit, machine-readable form: `user` for the caller and `public` for anonymous visitors. Values are `read`, `write` (PUT, PATCH, DELETE) and `append` (POST).
- **`Link rel="edit"`** is sent only to callers who can write. On the HTML view it points to the `.md` source URL. On the `.md` view it points to itself. `rel="edit-form"` points to the browser editor and `rel="create-form"` to the new-page form.
- **`Accept-Patch: text/x-diff`** is sent only to callers who can write, and only once PATCH exists (Phase 6). `Accept-Post` is sent only to callers who can write.
- **405 and 403 responses** always include `Allow`. A 401 includes `WWW-Authenticate`.
- **Caching:** these responses differ by user and format, so they carry `Vary: Accept, Authorization, Cookie`. Private spaces and authenticated responses add `Cache-Control: private`.
- Asset URLs get `Allow: GET, HEAD, DELETE, OPTIONS` (DELETE only for editors) and `WAC-Allow`. They do not get `Accept-Patch`.

## 3. Authorization model

- **Server admin:** a global flag on the user. Can create and delete spaces, manage users, and administer every space.
- **Space roles** (per space, through `space_members`):

| Role | Can |
|---|---|
| viewer | read pages, assets, revisions |
| editor | viewer, plus create, edit, move and delete pages, upload and delete assets |
| admin | editor, plus manage members, space settings, import and export |

- **Effective access to a space:**
  1. Server admin: admin.
  2. Space member: the member's role.
  3. Everyone else (anonymous or logged in without membership): read-only if the space has `public_read`, otherwise no access.
- A space that is not readable returns 404, not 403, so its existence is not leaked. It is also left out of `GET /_api/spaces` and cross-space search.
- Enforcement lives in one middleware that resolves `(user, space) -> role`, and the store methods take that role-checked context, so no handler can skip it.

## 4. Data model (SQLite)

Files in the data directory:

```
data/wako.db          catalog: spaces, users, memberships, sessions, API tokens
data/spaces/<id>.db   one database per space (named by space id, so renaming a slug moves nothing)
```

**Catalog (`wako.db`)**
```
spaces(id PK, slug UNIQUE, title, public_read BOOL DEFAULT 1, created_at)
users(id PK, name UNIQUE, password_hash, is_admin BOOL)
space_members(space_id, user_id, role /* viewer|editor|admin */, PK(space_id,user_id))
sessions(id, user_id, refresh_hash, expires_at)
api_tokens(id, user_id, token_hash, name, created_at)
schema_migrations(version)
```

**Per-space database (`spaces/<id>.db`)**
```
pages(id PK, parent_id REFERENCES pages ON DELETE CASCADE, slug, title, body TEXT,
      position, version, created_at, updated_at,
      UNIQUE(COALESCE(parent_id,0), slug))
revisions(id PK, page_id, version, title, body, author, action, note, created_at)  -- Phase 6: UNIQUE(page_id, version)
changes(id PK, at, action, page_id NULL, path, old_path, title, author, version, note)  -- Phase 6: the log behind the feed, outlives deleted pages
assets(id /* sha256 */ PK, name UNIQUE, mime, size, width, height, data BLOB, created_at)  -- Phase 3: the media library
page_assets(page_id, name, asset_id, PK(page_id,name))                  -- Phase 3: a page's image bindings
links(src_page_id, kind, target, PK(src_page_id,kind,target))            -- Phase 4: links as written, resolved on read
pages.path                                                              -- Phase 4: denormalised full path, unique
pages_fts USING fts5(title, body, content='pages')                      -- Phase 4: kept in sync by triggers
schema_migrations(version)
```

Consequences of the split:
- **No `space_id` column** exists in space tables. Cross-space leaks are impossible in SQL, since a query can only see the database it was opened on. Isolation tests still cover it.
- Space databases are opened lazily and cached by id. Each uses WAL and one connection, so writes in different spaces do not block each other.
- Each database migrates itself independently, on first open.
- Creating a space inserts a catalog row and creates the file. Deleting a space removes the catalog row, closes the handle and deletes the `.db`, `-wal` and `-shm` files.
- Cross-space operations (global search, cross-space backlinks) query each readable space's database in turn and merge the results.
- Backup is per space (`VACUUM INTO`) plus the catalog. Export and import are per space as before.
- Catalog and space files are not updated in one transaction. Creation order (catalog row first, then file, with rollback of the row on failure) and a startup consistency check (catalog rows without files get an empty database, orphan files are reported) cover the gap.

## 5. Phases

Each phase ends with a shippable, tested binary.

### Phase 0 — Skeleton ✅ done
- `go mod init`; layout: `cmd/wakod`, `cmd/wako`, `internal/{store,render,server,auth,config}`, `ui/` (embedded).
- Config via flags and `WAKO_*` env vars: `--addr`, `--data-dir`, `--base-path`.
- Migration runner, Makefile and CI with cross-compile targets (`CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"`).
- **Exit:** `wakod` starts, creates `wako.db`, serves `/healthz`.

### Phase 1 — Spaces, pages API and content negotiation (MVP) ✅ done
- `store`: a catalog (spaces: create, list, delete) plus one database per space. Page CRUD, tree by `parent_id`, path resolution and slug validation (reserved names, no `.md` suffix) work on the space's own database.
- `render`: goldmark to sanitized HTML.
- Handlers for the page routes above, with `Accept` negotiation and `.md` suffix handling, ETag and `If-Match` (412).
- `HEAD` and `OPTIONS` on all routes, and the editability headers from section 2a (`Allow`, `Accept-Post`, `Link rel=edit|edit-form|create-form|type`, `Vary`, and `Content-Type` with `variant=CommonMark`). In this phase the caller is the local admin, so all methods are advertised.
- Until Phase 5, everything runs as a built-in "local admin" and the server only binds to localhost by default.
- Minimal HTML page template (title, rendered body, edit link).
- **Exit:** `curl /{space}/a/b.md` and `curl -H 'Accept: text/markdown' /{space}/a/b` return the same body. A browser gets HTML. PUT, POST and DELETE pass table tests. The same slug works independently in two spaces.

### Phase 2 — Web UI: spaces, navigation and editor ✅ done (browser-untested, see 5a)
- Space switcher and the `/` space list. Sidebar tree (collapsible), breadcrumb, view and edit modes.
- Editor with live preview, Ctrl+E to edit, Ctrl+S to save, Ctrl+B and Ctrl+I, Esc to exit.
- Create, rename, move and reorder (drag and drop). Space creation and settings screen.
- Dark mode, responsive layout, configurable site name and logo.
- **Exit:** a user can create a space and build and edit a page tree entirely in the browser.

### Phase 3 — Images and the media library ✅ done (browser UI untested)
- Each space has a **media library**: uploaded images with unique names. Pages refer to an image by its bare name, page-relative, as in `![diagram](diagram.png)`. They never embed a space slug, an id or a path.
- Each page keeps its own **bindings** (name to image). Saving a page binds every bare image name it uses to the library image of that name. Existing bindings keep their image even if the library changes later. Names with no library image stay unbound and render as written.
- HTML rendering resolves a page's bindings to the image's immutable URL. Moving or renaming a page, or renaming its space, needs no Markdown changes.
- Upload (`POST /{space}/_assets`, raw body or multipart), the type checks, size limit, serving, delete and `gc-assets` are described in 5a.
- The editor has an **Image…** dialog: search the library and click to insert, or upload (paste and drop also work). The library has its own page at `/{space}/_assets/` with search, upload, rename and delete.
- **Exit:** paste a screenshot, save, and the image renders in HTML while the Markdown view shows `![shot](shot.png)`. A second page can reuse it through the library. The same bytes in two spaces are stored and served independently.

### Phase 4 — Search and links ✅ done (browser UI untested)
- **Search:** FTS5 per space (porter stemming, bm25 with titles weighted ten times body text), snippets with `<mark>` highlights, typing-friendly prefix matching. `GET /_api/spaces/{space}/search?q=` and `GET /_api/search?q=[&space=]` (all spaces the caller can read, merged). The *Search* box in the header goes to `/_ui/search`, and **Ctrl+Shift+F** opens a search pane with live results.
- **Links:** ordinary Markdown links, relative (`[text](sibling)`, `[text](../up)`) or absolute (`[text](/space/docs/page)`). Links to pages that do not exist are shown in red at display time and lead to the "create this page" form. The source stays plain Markdown.
- **Backlinks:** the "Linked from" panel on each page (`GET /_api/spaces/{space}/pages/{path}/backlinks`), including pages in other spaces the caller can read.
- **Broken links:** `/{space}/?broken` and `GET /_api/spaces/{space}/links/broken`, with a "create page" link for each.
- **Moves keep links working:** moving or renaming a page (and its subtree) rewrites the links that pointed at it, and recomputes the moved pages' own relative links, in the same transaction as the move.
- **Exit:** searching finds title and body matches, and moving a page leaves no broken links.
- **Portability rule for later phases:** import and export (Phase 7) copy page text unchanged. Images are the one Wako-specific part: `![alt](name)` names a library image, so an export must bundle the images and rewrite those names to file paths.

### Phase 5 — Auth and per-space authorization ✅ done (browser UI untested)
- Argon2id (`golang.org/x/crypto`). Bootstrap server admin from `--admin-password` or env, plus a `wakod reset-admin-password` subcommand.
- Short-lived access token plus refresh token in Secure, HttpOnly, SameSite cookies. CSRF token on state-changing requests. Login rate limiting. Per-user API tokens (`Authorization: Bearer`) for the CLI and scripts.
- Space membership with viewer, editor and admin roles, the `public_read` flag, and the access-resolution middleware from section 3. Anonymous visitors get read-only access to public spaces. A private space returns 404.
- Admin UI for users and members. `--allow-insecure` for plain HTTP, and `--disable-auth` for local use, with a loud startup warning.
- Make `Allow`, `Accept-Post`, `Link rel=edit` and `WAC-Allow` depend on the resolved role.
- **Exit:** a table-driven test covers every (anonymous, non-member, viewer, editor, space admin, server admin) × (public, private space) × (read, write, assets, members) combination. The same table asserts that the `Allow` and `WAC-Allow` headers match what the server actually permits (no advertised method may return 403, and no permitted method may be missing).

### Phase 6 — Revisions, conflicts and the change feed ✅ done (browser UI untested)
- **History:** every content change keeps a revision (text, title, author, time, a note). `GET /{space}/{page}?history` lists them (HTML, or JSON with `Accept: application/json`), `?rev=N` shows one (as HTML, Markdown or JSON, and `.md?rev=N`), `?diff=N[&from=M]` shows what a revision changed, `POST …?restore=N` makes an old text the current one as a new revision. API: `/_api/spaces/{space}/pages/{path}/revisions` and `/diff?from=&to=`.
- **`PATCH`:** a unified diff (`Content-Type: text/x-diff`) applied to the version named in `If-Match`. Missing `If-Match` is 428, a stale version 412, a diff that does not fit 409 with the reason. `Allow` lists PATCH and `Accept-Patch: text/x-diff` is sent, to writers only.
- **Conflicts:** saving from the editor sends `If-Match`, so the second of two simultaneous saves is refused with 412. The editor then fetches the current text, shows the difference (their version → yours) and offers *Save mine over theirs*, *Use theirs* or *Keep editing*. Nothing is overwritten unless the author chooses it, and even then the other text stays in the history.
- **Change feed (RSS 2.0):** `/{space}/_feed` for one space and `/_feed` for every space the reader can see. One entry per change, newest first, with title, absolute link, author (`dc:creator`), category and an HTML description that carries the diff for edits, an excerpt for new pages, and the old and new place for moves. Browsers find it through `<link rel="alternate">`, and `/{space}/?changes` shows the same list as a page. `/_api/changes` and `/_api/spaces/{space}/changes` give it as JSON.
- **Exit:** two simultaneous edits never silently overwrite each other.

### Phase 7 — Export, import and the command line ✅ done (UI untested in a browser)
- **Export:** `GET /_api/spaces/{space}/export` (space admins) returns a zip. Each page is the `.md` file you wrote, byte for byte, laid out as the page tree. **Each image sits next to the pages that use it**, so the Markdown works in any viewer with no rewriting. A small `wako.json` holds the page order, titles and image checksums. Images no page uses go in `_library/`.
- **Import:** `POST /_api/spaces/{space}/import[?conflict=skip|overwrite&under=PATH]` takes that zip, or any folder of Markdown zipped up. See the notes below for what it cleans up.
- **Command line:** `wako` is a second static binary (7 MB, no database code). `spaces`, `ls`, `get`, `put`, `rm`, `mv`, `search`, `upload`, `export`, `import`, with `--server`, `--token`, `--space` or `WAKO_SERVER`, `WAKO_TOKEN`, `WAKO_SPACE`. `wako import FOLDER` zips a folder itself.
- **Backup:** `wakod backup -data-dir D -to DIR` writes a consistent copy of every database while the server runs. The copy is itself a data directory.
- **Settings page:** space admins get a download link and an import form.
- **Exit:** exporting a space and importing it into an empty one gives an identical tree, texts, order, links, image bindings and image bytes (tested, including through the CLI and over HTTP).

### Phase 8 — Hardening and packaging ✅ done (Docker image verified; goreleaser, CI, nginx, Caddy and systemd files not run here)
- Security: HTML sanitization tests, startup consistency check between catalog and space files, CSP headers, path traversal and slug fuzzing, upload abuse tests, cross-space isolation tests (no page, asset, link or search result leaks between spaces), body-size limits, request timeouts.
- Base-path support behind a reverse proxy, graceful shutdown, structured logging, optional TLS flags.
- Release pipeline: goreleaser for linux, macOS and Windows on amd64 and arm64, checksums, and a `FROM scratch` Docker image. Systemd unit and README with an nginx example.
- Benchmarks (for example 10 spaces × 10k pages) and an SQLite tuning pass.
- **Exit:** v1.0 tag, tested on a clean machine with no installed dependencies.

## 5a. Status

Phases 0 and 1 are implemented (`make test`, `make build`, `make cross`). Notes:
- Every page advertises `POST` to writers, not just pages with children, because creating a child turns a page into a container. Only `rel="type"` (BasicContainer) depends on having children.
- A bare-bones `?edit` and `?create` form (a textarea that saves with `fetch`) exists so the `edit-form` and `create-form` links resolve. The real editor is Phase 2.
- `?reorder` takes `{"order":[slugs]}` and sets the order of a page's children (Phase 2).
- `PATCH` and `Accept-Patch` arrive with Phase 6.
- Until Phase 5 every caller is the local admin, so `wakod` binds to `127.0.0.1` by default. Do not expose it.

Phase 2 notes:
- The UI is server-rendered HTML plus one embedded `app.js` (no framework, no build step). The sidebar tree is in every page, so HTML responses have no ETag. Markdown and JSON keep theirs.
- Added: `PUT /_api/spaces/{slug}` (title, slug, `public_read`), `POST /_api/spaces/{slug}/render` for the live preview, `/_ui/new-space`, `/{space}/?settings`, `--site-name` and `--logo`.
- **Not verified in a real browser.** Go tests cover the HTML, the API calls the drag-and-drop makes, and `app.js` passes `node --check`, but the interactive behavior (editor shortcuts, drag and drop, dialogs, mobile drawer) still needs a manual pass.
- **Mermaid diagrams are deferred**: vendoring `mermaid.min.js` adds about 3 MB to the binary and fits better with Phase 4's rendering work.

Phase 3 notes:
- **Names:** library names are unique per space. A name taken by different bytes gets a suffix (`shot.png`, then `shot-2.png`). Identical bytes are stored once and keep their first name. Names are cleaned to `[A-Za-z0-9._-]` and the extension follows the detected type.
- **Upload:** `POST /{space}/_assets` takes a raw image body (`?name=`) or `multipart/form-data` (field `file`). It returns `{id, name, mime, size, url}`: 201 if new, 200 if the bytes already existed. `name` is what Markdown should use.
- **Type checks:** the type comes from the content, never from the client. Only png, jpeg, gif, webp and svg are accepted. SVG needs a `.svg` name and an `<svg` element, and is served with `Content-Security-Policy: … sandbox`. Every asset gets `nosniff`.
- **Lookup:** `GET /{space}/_assets` lists the library (JSON with `?q=` search and `used_by`, or the gallery page). `GET /{space}/_assets/{name}` redirects (302) to the immutable `/{space}/_assets/{id}/{name}` URL, which has an ETag, range support and `immutable` caching (`private` for private spaces).
- **Delete:** `DELETE /{space}/_assets/{id}` returns 409 while pages use it, unless `?force=true`.
- **What counts as a use:** only bare names in image syntax, found by parsing the Markdown, so images in code blocks do not count. Absolute image URLs still display but are not tracked, so the image can be deleted or collected while a page links to it.
- **Preview:** the live preview resolves names through the page's bindings first (`?page=`), then through the library, so a name you just uploaded shows before the first save.
- **Library cleanup:** library images are meant for reuse, so unused ones are kept. `wakod gc-assets [-data-dir D] [-older-than 24h]` is an explicit command that deletes images no page uses.
- **Raw Markdown clients:** a bare `shot.png` does not resolve as a URL against `/space/page.md`. A client that wants the image can call `GET /{space}/_assets/shot.png`.
- **Migration:** data from the first Phase 3 build upgrades in place (duplicate names get an id prefix). Pages that used absolute asset URLs there are unbound until saved again with a bare name.
- **Rename:** `POST /{space}/_assets/{id}?rename=new-name.png` (or the *Rename* button on the library page) gives a library image a new name and rewrites the reference in every page that uses it, in one transaction. It returns `{…, pages_updated, pages_aliased}`.
  - Names follow the upload rules. The extension must fit the type (`.jpg` or `.jpeg` for JPEG), and a name with no extension gets the usual one. A name already used by a different image gives 409, and the same name is a no-op.
  - The rewrite only touches real image syntax: inline `![alt](name "title")`, `<name>` destinations and `[ref]: name` definitions. It leaves code blocks, code spans, raw HTML, links, paths and other names alone, even ones that share a prefix.
  - It then checks its own result against the parsed document. If a page cannot be rewritten safely, or already binds the new name to something else, that page keeps working: its binding stays under the old name as an **alias** to the same image, and it is counted in `pages_aliased`. The old name is then free for a different image without affecting that page.
  - Rewritten pages get a new version, so an editor that still has the old text open gets a 412 conflict on save instead of silently undoing the rename. Phase 6 revisions should record the rename as a change.
  - A page that mentions the new name without a binding (the image did not exist when it was saved) is rewritten to the same name but stays unbound until its next save.
- **Not yet done:** a dry-run for `gc-assets`, and a rename preview that lists the affected pages.

Image pipeline (`internal/img`, standard library only; adapted from the Weft project):
- **Cleaning at upload:** metadata is always stripped (EXIF and GPS, XMP, comments, thumbnails, vendor blocks, text chunks, trailing data). The colour profile is kept. This works on the file structure for JPEG, PNG, GIF and WebP, so untouched pictures are not recompressed. SVG is stored as uploaded.
- **Orientation:** a JPEG with an EXIF orientation is rotated and recompressed once (quality 92), because the flag is metadata.
- **Compact mode (default):** a JPEG or PNG whose longest side exceeds 1600 px is shrunk (JPEG quality 80, profile re-attached), and the smaller of the shrunk copy and the cleaned original is kept. `?mode=original` on upload, or `--image-mode original`, only strips. GIFs, animated PNGs, WebP and SVG are never re-encoded, because re-encoding would drop animation or there is no encoder.
- **Limits (all configurable):** `--max-upload-mb` 25 in, `--max-stored-mb` 5 kept, `--image-max-mpix` 50 megapixels (an iPhone photo is 12, 24 or 48) decoded (checked from the header before decoding, so a small file cannot unpack into a huge bitmap), `--image-max-side` 1600, GIFs at most 300 frames. Over-limit uploads get 413, undecodable ones 400, and an unsupported type 415.
- **Sizes are stored:** width and height are saved with each asset, and older assets are backfilled when their space opens, so layout never reads image bytes.
- **Quota:** `--space-quota-mb` (default 200, negative means unlimited) is enforced inside the `INSERT`, so concurrent uploads cannot overshoot it. A full space answers 507, and re-uploading known content still succeeds. The library page shows usage.
- **Copies on request:** `GET …/_assets/{id}/{name}?w=N` (N pixels wide), `?h=N` (N pixels high) or `?s=N` (longest side N; used for the default page picture, 800, and thumbnails, 240). N must be one of 60 (thumbnail), 240, 400, 600, 800, 1200, 1600, only one of the three may be given, anything else returns 400. A copy is made only for a JPEG or PNG whose longest side exceeds the copy's and that is larger than 100 KB (default page picture) or 20 KB (every other request), and only served if it is really smaller. Copies live in a 64 MB in-memory LRU keyed by the longest side, concurrent requests share one build, and at most two decodes or scalings run at once (uploads share those slots). A matching `If-None-Match` (`"<id>-w<longest side>"`) gets a 304 without building anything. Copies are immutable, so they are cached forever.
- **Sizes in pages:** a page writes `![alt](name.jpg#w=400)` or `#h=300` (a fragment, so other Markdown viewers still find the file; the older `?w=`/`?h=` form is read too, and the `=` is optional (`#h60` is `#h=60`), and requests to the server such as `…/_assets/{id}/{name}?w=N` and `/_resize?w=N` keep real query parameters); the suffix is part of the reference (renames keep it, imports and exports carry it). The renderer passes it to the server's resolver, which links a built copy to the original; for a GIF, WebP, SVG or small file the original is shown with `width`/`height` attributes so the browser scales it. An unlisted size is ignored (the default applies). A size asked for is the size shown: a picture (or its copy) smaller than that is enlarged by the browser, through exact `width`/`height` attributes for library pictures, `width` for `#w=` and a `--h` style variable for `#h=` on other addresses. On any other image address (`https://…/logo.svg#w=400`) the fragment is removed and becomes a `width` or `height` attribute, any 2–4 digit value, with no fetch or resize. The editor's **Image…** dialog has a size picker, and the AI prompts know the syntax.
- **Pictures on other sites (`/_resize`):** `GET /_resize?w=N|h=N&img=<http(s) address>&space=<slug>&sig=<tag>` (N from the same ladder). A page writes `![alt](https://…/photo.jpg#w=N)` (or `#h=N`); when it is rendered, `externalResizer` (passed to the renderer as `Options.External`) replaces the address with the signed one and links the picture to the original, but only for an http(s) address whose path ends in .jpg, .jpeg or .png, a size on the ladder, and a host that is not a blocked literal address; otherwise the fragment becomes a `width` or `height` attribute and nothing is fetched. The signature is made at render time (`auth.Signer.Tag("resize", "<space>|<dim>|<n>|<address>")`, 16 bytes of HMAC-SHA256 under the signing key, so a link cannot be moved to another space; with sign-in off there is no signature), so it covers whatever editors wrote into pages. Two switches, both of which must be on: the server-wide `-external-resize` (default on) and the space's `external_resize` column (default on, set by a space administrator under Settings or `PUT /_api/spaces/{slug}`); with either off no signed address is made (the picture is shown at the size asked for), existing ones are refused (403), no fetch happens, and the editor hides the box. The cache key leaves the space out, so spaces share copies. The fetch refuses private, loopback, link-local, CGNAT and other non-public addresses at dial time (`Dialer.Control` sees the resolved address, so DNS names and redirects are covered), ignores proxy settings, follows at most 3 redirects, reads at most 10 MB in 8 seconds, believes the bytes and not the content type (JPEG or PNG only), and checks the pixel limit before decoding. The resized copy lives in the in-memory variant cache (64 MB, shared with library copies) under a key that includes the day, so it is rebuilt at most daily; the original is dropped. A matching `If-None-Match` is answered with 304 without any fetch.
- **PDF attachments:** a PDF goes into the same media library, as an asset with mime `application/pdf` and no dimensions. `img.Sniff` recognises it by its signature (never by name or header), `img.Process` only passes it through, `-max-upload-mb` limits the upload and `-max-pdf-mb` (10 MiB) what is kept, separately from `-max-stored-mb` for images (no shrinking is possible, so a PDF over its limit is refused). Nothing is rendered or rewritten inside it. A page links to it with `[text](name.pdf)`: a bare name ending in `.pdf` is bound to the asset on save like an image name (`render.ImageRefs`), rendered as a link to the file, and is never a page link (it is not in `Links()`, never broken, and `wiki.Link` ignores any path ending in `.pdf`). Page slugs may not end in `.pdf`, so `/{space}/{dir}/name.pdf` serves the attachment bound by a page in that directory. PDFs are served `inline` with `Content-Disposition` under their name, `nosniff`, and a CSP of `frame-ancestors 'self'` (no `sandbox`, because browsers will not display a sandboxed PDF). Library listing, rename (the link in pages is rewritten), delete, quota, export (next to the pages that use them) and import (including relative links in plain Markdown folders) treat them like images.
- **Page HTML:** every library image gets `width`, `height` and `--r` (aspect ratio), so the browser reserves space and the page does not shift. A page shows the 800 px copy for large pictures, wrapped in a link to the original (unless already inside a link). Every image gets `decoding="async"`, and every image after the first gets `loading="lazy"`. CSS narrows tall pictures to 85% of the viewport height. The library and the editor's image dialog use 240 px thumbnails.
- **Links:** internal links no longer carry `rel="nofollow"`; absolute external links still do.
- **Not measured:** these changes are meant to help Lighthouse (layout shift, image size, lazy loading), but no Lighthouse run has been made on this project yet.

Phase 4 notes:
- **Shortcut, not syntax:** `[[Page]]` and `[[Page|label]]` are expanded to ordinary Markdown links when a page is saved (PUT, POST, PATCH, accepted suggestion), so stored text never needs wako to be read (`wiki.ExpandShortLinks`). A name matches a page by title or last path segment, ignoring case (a page beside the current one wins); `a/b` is a path from the space root; no match means a new page beside the current one. Code, fenced blocks, `\[[` and `![[` are left alone. Nothing is tracked as wiki syntax.
- **Links resolve** as a browser would from the page's URL: `sibling` is the page next to this one, and `docs/ops` from `faq` is `/docs/ops`. External URLs, `#anchors`, `_assets` paths and links that leave the space are not page links.
- **Resolved on read:** the `links` table stores links as written, not where they pointed. Backlinks and the broken-link report resolve them when asked, so creating, deleting or moving a page needs no re-indexing, and nothing goes stale.
- **What a move rewrites:** relative links are recomputed from the page's new location, absolute ones get the new path, and `[ref]: dest` definitions too. A link that still reaches its target from where the page now is is left exactly as written. Code, raw HTML, images and external links are never touched.
- **Not rewritten:** links from other spaces. Rewriting them would edit pages the person moving the page may not be allowed to write; they show in that space's broken-link report instead. Renaming a space slug rewrites absolute links inside that space only. After a rename, links to `/oldname/...` in other spaces start with a name that is no longer a space, so they look like ordinary site paths and are not reported.
- **Versions:** rewritten pages get a new version, so an editor that has the old text open gets a 412 on save. Phase 6 revisions should record these changes.
- **Search details:** the query is split into words (punctuation and FTS operators are dropped, at most 12 words), every word must match, and the last also matches as a prefix. Snippets show the page's Markdown source, not rendered text. The highlight markers are random per process, so page text cannot forge a `<mark>`. Scores from different spaces are merged by bm25, which is only roughly comparable across spaces.
- **Upgrade:** older databases get `pages.path` filled in and the search index built when the space first opens.
- **Not yet done:** a preview listing what a move will rewrite, and rewriting links in other spaces (with Phase 5, for admins).

Phase 5 notes:
- **Default is closed:** sign-in is on unless `--disable-auth` is given, and the server prints a loud warning when it is off. On first start with no administrator it creates `admin` (`--admin-user`) with `--admin-password` / `WAKO_ADMIN_PASSWORD`, or a random password printed once. `wakod reset-admin-password [-user admin] [-password …]` sets a new password from the console (creating an administrator if the name is new) and ends that user's sessions.
- **Rules** (the exit-criterion matrix tests exactly these): a server administrator can do everything; a member gets their role (viewer reads; editor also writes pages and images; space admin also manages members and the space's settings); everyone else, signed in or not, can read a space only if it is public. A space the caller cannot read answers 404 to everything, including writes, so its existence is not revealed. A missing right is 401 for anonymous callers and 403 for signed-in ones, and both carry `Allow`.
- **Server administrators** create and delete spaces and manage users. **Space admins** edit title, slug and public access, and manage members.
- **Sign-in:** Argon2id passwords (64 MiB, 3 passes), 8–256 characters. `POST /_api/login` sets three cookies: a signed 15-minute access token and a 7-day refresh token (both `HttpOnly`, `SameSite=Lax`), and a CSRF token the page script reads. The refresh token rotates on use, and the old one stays valid for 30 seconds so requests already in flight (a page and its images) do not fail. A session that sits unused for 7 days ends. Signing out, changing a password, or an administrator resetting it ends sessions at once, because every request also checks the session row.
- **CSRF:** every cookie-authenticated change (POST, PUT, PATCH, DELETE) must send `X-CSRF-Token`, an HMAC of the session id. Requests with an API token are exempt, since a browser never adds those by itself.
- **Cookies and HTTP:** `Secure` over HTTPS (directly or via `X-Forwarded-Proto: https`). Over plain HTTP, cookies are only set for `localhost` and `127.0.0.1`, or anywhere with `--allow-insecure`. Otherwise sign-in is refused with a message saying so.
- **Brute force:** 5 failures per (client address, user name) and 30 per client address in 15 minutes, then 429 with `Retry-After`, even for the right password. Unknown names cost the same time as real ones. The limiter is in memory and keyed on the connection's address; behind a reverse proxy that is the proxy's address (no `X-Forwarded-For` is trusted), so the per-name limit is what protects accounts.
- **API tokens:** created from the Account page (shown once, only a hash is stored), sent as `Authorization: Bearer wako_…`, and valid with exactly their owner's permissions. A wrong Bearer token is a 401 even on public pages. Tokens cannot create tokens or change passwords. Other `Authorization` schemes are ignored, so a reverse proxy's Basic authentication passes through.
- **Safety nets:** the last administrator cannot be demoted or deleted. Open redirects after sign-in are blocked (only paths on this site, no control characters).
- **Settings:** `--access-token-timeout` and `--refresh-token-timeout` take `15m`, `36h`, `7d`. `--allow-insecure`, `--disable-auth`, `--admin-user`, `--admin-password`, all also as `WAKO_*` variables.
- **Not yet done:** a list of someone's active sessions with remote sign-out, per-token scopes (read-only tokens), two-factor sign-in, email password resets, and trusting a proxy's `X-Forwarded-For`.

Phase 6 notes:
- **What counts as a revision:** any change to a page's title or text: a save, a restore, a PATCH, and the edits Wako makes itself (links rewritten after a move, an image renamed), which carry a note such as "links updated after moving /a to /b" and the name of whoever caused them. Moving a page changes the change log but not its text, so it adds no revision. Existing pages start their history at the version they have; the feed starts with the 50 most recently changed pages.
- **Merging quick saves:** saves from the **browser editor** by the same person within `--revision-window` (default 10 minutes, `0` turns it off) of their last one replace that revision instead of adding another, and the feed entry is updated in place. Saves made with an **API token**, by `PATCH`, or by restoring are never merged, because each call is deliberate. With sign-in off, saves count as browser saves. A merged-away version number stops existing, so `?rev=N` for it is a 404.
- **Retention:** `--keep-revisions` (default 100, `0` keeps all) revisions per page; the oldest go first. A revision stores the full text, not a delta. The change log keeps the latest 1000 entries per space, and an entry never keeps the id of a deleted page, because SQLite may reuse ids.
- **Who can see history:** anyone who can read the page, which on a public space includes anonymous readers. Authors' user names are shown in history and the feed. Restoring and PATCH are writes; a private space's history and feed are as hidden as the space.
- **Diffs:** a line diff (Myers, minimal), cut off to a plain replacement for texts that differ by more than about 1500 lines. `Apply` is strict: every context and removed line must match where the diff says; it never guesses. It reads patches from `diff -u` and git, ignores file headers, and stops a hunk when its line counts are used up. A test checks both directions against the system `diff` and `patch` programs.
- **Feed readers and private spaces:** feed readers cannot send Bearer headers, so a feed also accepts **HTTP Basic with any user name and an API token as the password** (`https://reader:wako_…@host/space/_feed`). Anonymous requests for a private or unknown space get the same 401 challenge, so nothing is revealed. Basic is honoured on feed URLs only; elsewhere it still belongs to a reverse proxy.
- **Absolute links:** a feed needs them. Wako uses the request's scheme and host (and `X-Forwarded-Proto`), or `--public-url https://wiki.example.com` when behind a proxy that changes them. Feeds are cached for 5 minutes (`public` for public spaces, `private` once credentials are involved) and answer `If-None-Match` with 304.
- **Not yet done:** collapsing the many entries a big move or rename can produce into one, a per-page feed, Atom output, and showing a side-by-side diff.

Phase 7 notes:
- **Not in scope, by decision:** revisions in the export (it carries current texts only), and LeafWiki import. Wako never adds front matter to pages; order and metadata live in `wako.json`. Front matter that people write is read (see Markdown dialect).
- **Layout:** page `docs/ops` is `docs/ops.md`; its children are in `docs/ops/`. An image used by `docs/ops` is `docs/shot.png`, next to `docs/ops.md`, since a relative name in that page resolves to its directory. The same picture used by pages in several folders is written once per folder; import stores it once. If two sibling pages use one name for two different pictures (possible only after a rename that could not be rewritten), the second is written under a new name and its text adjusted, with a note in `wako.json`.
- **What import accepts:** `.md` files become pages. A folder's `index.md` or `README.md` is the folder's own page, unless a `folder.md` exists. File and folder names are turned into valid page names (`My Notes.md` becomes `My-Notes`, with the original as its title when it has no heading), and a folder with no page of its own gets an empty one titled with the folder's name. Siblings are ordered alphabetically, ignoring case, or by `wako.json` when there is one. A UTF-8 byte-order mark is removed; line endings are kept.
- **Images on import:** each goes through the same cleaning as an upload (metadata stripped, large ones shrunk, limits applied). A relative image path in a page (`images/pic%20one.png`, `<images/pic one.png>`, `./Pic.PNG`) is rewritten to the picture's library name, which may differ if that name was taken by another picture or the type forced another extension. The same bytes in several folders are one library image.
- **Left out, quietly or with a warning:** hidden files and folders (`.git`, `.obsidian`, `.DS_Store`, `__MACOSX`), other file types (counted and named in the report), pages that are not valid UTF-8, files over the size limits, images that are not really images. Links between pages are not rewritten, so a link such as `[x](My Notes.md)` to a renamed page shows up in the broken-link report.
- **Safe against hostile zips:** entries with `..`, absolute or drive-letter paths, NUL bytes or backslash tricks are refused; symbolic links and duplicate names are skipped; the number of entries, the unpacked size (4x `--max-import-mb`), each page and each image are limited; and the checksums in `wako.json` are verified before anything is written. A damaged or too-large archive changes nothing. An import that fails partway (the media quota filling up, say) keeps what it already added and the error response carries the report.
- **Conflicts:** pages that already exist are skipped by default; `overwrite` replaces their text as a new revision (the old text stays in the history). Imported pages are credited to whoever imported them, with the note "imported".
- **Limits and time:** `--max-import-mb` (default 256) bounds the upload. Export and import lift the server's request timeouts, since moving a large space takes a while.
- **Who may:** space admins (and server admins), as for settings. Export includes everything in the space, so it is not offered to readers.
- **Backups hold secrets:** password hashes, session records and the signing key. Keep them private.
- **Not yet done:** exporting only part of a tree, link rewriting on import, exporting history, restoring a backup into a running server (stop it, replace the data directory, start it).

Phase 8 notes:
- **Request pipeline** (`internal/server/hardening.go`): panic recovery (500 page, stack in the log), a request id on every response, one structured access-log line per request (`-access-log=false` silences it), security headers (CSP with no inline script, `nosniff`, `Referrer-Policy`, `frame-ancestors 'none'`), then the host and cross-site checks, then routing.
- **Host check (DNS rebinding):** requests for a host name the server was not told about get 421. Without `-allowed-host`/`-public-url`, only local names and IP addresses are answered; a page served by an attacker's DNS name that resolves to 127.0.0.1 therefore cannot read the wiki. With sign-in on, the host must also be `-public-url`'s or an allowed one.
- **Cross-site writes:** unsafe methods with `Origin` or `Sec-Fetch-Site` that say "another site" are refused (403). Cookie sessions also need the CSRF header; Bearer tokens do not, since a browser never sends them by itself.
- **Trusted proxies:** `X-Forwarded-For/-Proto/-Host` are believed only from `-trusted-proxies` (default: this machine). The login rate limit uses the real client address. This resolves the earlier caveat that all proxied users shared one limit.
- **Startup checks:** a space whose file is missing stops the server (it would look empty); `wakod forget-space SLUG` removes the record; `wakod spaces` lists which file (named by space id) holds which space. Stray files in `spaces/` are reported, never deleted. A data directory readable by others warns. Directories are created `0700`.
- **`wakod check [-deep] [-repair]`:** SQLite `quick_check` (or `integrity_check`), foreign keys, search-index comparison with the pages, and stored paths against the tree. `-repair` rebuilds the search index and the stored paths, the two things derivable from other data.
- **Listening:** TCP, `unix:/path` (mode 0660) or HTTPS with `-tls-cert/-tls-key`; `wakod healthcheck` works for all three. On SIGTERM the server stops accepting, waits up to 30 s for requests in flight, then closes the databases.
- **Tests added:** an XSS corpus and a structural HTML check (`internal/render/safety_test.go`); hostile titles, space names and Markdown on every page that shows them; a private space that never leaks through pages, revisions, APIs, search, feeds or export (with a member as the control that the content is there); the README lists every flag; the real binary over TCP, TLS and a Unix socket, with graceful shutdown and a missing space file.
- **Fuzzing** (`make fuzz`; 12 targets): image processing and scaling, patch apply and diff round trip, link rewriting and resolution, page paths, search text, zip import, arbitrary request paths against the whole server, and the renderer. About 20-60 s each was run. It found two real bugs, both in WebP handling: a crafted VP8X chunk crashed the cleaner, and the size was read from bytes after the declared end of the file. Both are fixed; the seeds stay in `testdata/fuzz` as regression tests.
- **Performance** (one space, 10,000 pages, Apple M4): read a page 0.17 ms, save 0.7 ms, search for a rare word 0.3 ms, search for a word on every page 56 ms (ranking 10k matches), build the page tree 27 ms. The tree is needed for the sidebar of every page view, so it is now cached per space and invalidated by SQLite's `total_changes()`; a cached lookup is 0.12 ms. Not measured: 10 spaces at once (spaces are separate files and connections, so they do not contend), and the cost of rendering a very large sidebar in the template.
- **Packaging:** `make release VERSION=x` writes tar.gz/zip archives and `SHA256SUMS` (tried; the linux binaries are static ELF). `Dockerfile`: `FROM scratch`, 22 MB, user 65532, `/data` volume mode 0700, `HEALTHCHECK wakod healthcheck`; built and run here (health `healthy`, data survives a restart, clean stop). `.goreleaser.yaml`, `.github/workflows/{ci,release}.yml`, `deploy/{wakod.service,nginx.conf,Caddyfile,wako.env.example}` are written but were **not run** (the tools are not installed here).
- **Icon:** `internal/server/ui/icon.svg` is the favicon and brand mark unless `-logo` is set.
- **Not done:** a 1.0 tag, and a test on a machine that is not this one. The browser UI has still never been tried in a real browser.

### Phase 9 — Suggestions and AI integration ✅ done (browser UI untested; no live model tried)
- **Suggest-only access.** New role `suggester` (read + propose) and a `suggest_only` flag on API tokens, which caps whatever the token's owner may do. `Access.Suggest`; a write from someone with Suggest but not Write is queued as a proposal and answered `202 Accepted` with `Location: /{space}/_proposals/{id}`. `PUT`, `PATCH` and creating a child page can be proposed; moving, deleting, restoring and reordering cannot. Responses carry `Wako-Mode: suggest`. A full queue (200 pending) answers 429.
- **Review.** `/{space}/_proposals[/{id}[/accept|reject|withdraw]]`, HTML or JSON by `Accept`. Reviewers (writers) see all; suggesters their own; readers none. Accept replays the change onto the current text if the page moved on (`diff.Apply`; 409 if it no longer fits), or takes the reviewer's own text. The saved revision is credited to the suggester, with a note naming the reviewer and, for AI drafts, the model. Each proposal is one row in the space's own database (`proposals`), with the base text it was written against.
- **MCP** (`internal/mcp`): JSON-RPC core over stdio (`wako mcp`) and streamable HTTP (`POST /_mcp`, Bearer token only, never a cookie session). Tools run as ordinary API requests with the caller's token, so every access rule applies unchanged; `/_mcp` re-enters the server's own handler, so host checks, rate limits and the access log apply too. 10 tools; none accepts a suggestion or deletes. `write_page` without `expected_version` only creates; `edit_page` needs `old_text` to occur exactly once.
- **Provenance.** Direct writes record "via MCP", "via API token NAME" and the client's stated reason (`X-Wako-Source`, `X-Wako-Note`; the client's own statement, shown as such).
- **Ask the wiki and drafts** (`internal/ai`, `server/ai.go`): provider interface with the Anthropic Messages API and any OpenAI-compatible endpoint (also covers Ollama; a local URL needs no key). Off unless configured and switched on per space by a *server* administrator (page text leaves the server). Retrieval is FTS5 with an OR query of the question's keywords (bm25), at most 6 pages and 30k characters, from readable AI-enabled spaces only. Prompts mark pages as untrusted data and neutralise wrapper tags; citations `[n]` are linked by the server to the pages it actually supplied; images are removed from answers and external images from drafts (no beacon through the model); a question with no matching pages never reaches the model. Per-user hourly limit, 4 concurrent calls, the write deadline lifted for these requests only. Errors are worded for users and never contain the key.
- **Where it shows:** `/_ui/ask` (two tabs), an Ask button in the header, a review screen, a "n to review" badge for reviewers, "Suggest an edit" for suggesters (the editor shows "Sent as a suggestion"), a suggest-only checkbox for tokens, the AI switch in space settings, and the CLI commands `ask`, `draft`, `suggestions`, `suggestion`, `mcp`.
- **Tests added:** suggestion flow and permissions, rebase and conflict, history credits, queue limit, escaping on review pages; MCP protocol, tools, token limits, private spaces; AI with a fake model (what reaches it, who may call it, rate limit, failures, draft → save paths); provider clients against a fake HTTP service; CLI over a real server and real stdio; the private-space leak test now covers suggestions and `/_api/ai`. Fuzz targets: MCP handler, citation linking, draft cleaning, keywords, the OR query. Fuzzing found two small bugs (zero-padded citation markers; a lone carriage return in drafts), both fixed.
- **Editor assistant** (`server/ai_edit.go`, `ai.EditPrompt/ParseEdit`, the box under the editor): `POST /_api/ai/edit` takes the page in three parts (before, selected, after; no offsets, so UTF-16 vs UTF-8 cannot disagree). The model starts its reply with `@@replace` or `@@new`; *where* the text goes is decided by the server (selection or whole page for a replace; after the selection / at the cursor if the instruction says here, insert or cursor; otherwise the end). A replace that was cut off, empty, or aimed at a page too long to send whole (30k characters) is refused, because applying half a rewrite would delete the rest. Edge whitespace of the replaced text is kept. External images are stripped. The browser applies it to the textarea with its own undo stack (Undo button, Ctrl+Z while nothing was typed since), and drops the answer if the text changed while it waited. Nothing is saved.
- **Markdown dialect:** GFM tables, strikethrough, task lists (as emoji) and autolinks are on; the AI prompts now ask for GFM.
- **Front matter, macros, kanban:** `render.FrontMatter` reads a flat YAML subset (no anchors or tags) at the very start of a page, only if every line between the `---` fences is `key: value` or belongs to the key above (otherwise it is ordinary Markdown), at most 16 KB, 64 keys, 2000 characters a value. A block parser at priority 0 swallows it so links, pictures and excerpts never see it; `render.Title` prefers its `title` and skips it when looking for a `# ` heading; the change feed strips it from excerpts. Macros `{{page.NAME}}` (title, path, slug, space built in; any other front matter key) are expanded after parsing on the text nodes only (not code spans, code blocks or raw HTML), as `ast.String` nodes written with `SetRaw`, so a value is escaped and never parsed as Markdown; path, slug and space cannot be overridden by front matter; no value is expanded again; at most 128 KB of values go into one page. `goldmark-kanban` renders `<!-- kanban -->` regions and `kanban: true` pages as `div.kanban > section.kanban-column > ul.kanban-cards > li.kanban-card`; bluemonday allows exactly those classes, and the task box is still the emoji (our renderer outranks the extension's).
- **Not done / by decision:** no embeddings or semantic search (idea 3 was not chosen); no streaming of answers; no conversation memory in Ask (each question stands alone); no OAuth for MCP (a Bearer token is used); suggestions have no comments thread and send no notification beyond the badge; AI settings are server-wide (one model), not per space; **no live model was called**, only fakes and fake HTTP services, so prompt quality is untested.

## 6. Testing strategy
- Unit: store, slug and path rules, link extraction, sanitizer, access resolution.
- API: `httptest` suites per phase, including content negotiation, `.md` suffix and ETag behavior.
- Integration: start `wakod` on a temp dir, then drive it with the `wako` CLI.
- Browser smoke tests for the UI (a small Playwright script, kept outside the shipped binary).
- Check with `ldd` or `otool -L` that release binaries are fully static.

## 7. Remaining assumptions to confirm
1. A logged-in user with no membership in a space gets the same access as an anonymous visitor (read-only if public). Membership only raises access.
2. Space slugs are flat (no nested spaces) and can be renamed by a space admin. Relative asset and page links keep working after a rename.
3. `GET /` shows the space list. If you want one space to serve at `/`, a `--default-space` flag could be added.

## 8. Source notes
Taken from the LeafWiki README only (source code was not read). Its HTTP API, export format and role permissions are undocumented, and the README contradicts itself on roles. The space, role and API design above is my own.
