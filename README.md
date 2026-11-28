<p align="center"><img src="internal/server/ui/icon.svg" width="96" alt="wako"></p>

# wako

A wiki that stores plain, portable Markdown in SQLite. One static program, no runtime dependencies.

- **Markdown in, Markdown out.** `GET` with `Accept: text/markdown` (or a `.md` suffix) returns the text you wrote; a browser gets HTML. `PUT`, `POST`, `PATCH`, `DELETE` do what you expect.
- **GitHub Flavored Markdown, nothing more.** CommonMark plus tables, strikethrough, task lists (shown as ✅ and ⬜) and bare-URL autolinks. No footnotes, no raw HTML. Typing `[[Page name]]` (or `[[Page name|label]]`) is a shortcut: when you save, it becomes an ordinary link, `[Page name](page-name)`, to the page with that title or name, or to a new page beside this one, which is created for you. Your pages read the same in any Markdown viewer.
- **Front matter, macros and kanban boards.** A `---` block of `key: value` lines at the very start of a page is never shown: `title:` sets the page title, `kanban: true` turns the page into a board, and every key can be used in the text as `{{page.key}}`. The built-in macros are `{{page.title}}`, `{{page.path}}`, `{{page.slug}}` and `{{page.space}}`. They are filled in when the page is shown (the stored Markdown keeps them), they are plain text that can never become markup, and they are left alone in code. Write `\{{page.title}}` to show one as it is. A flat subset of YAML is read (plain, quoted, `[a, b]` and `- item` values); a block that is not of that shape stays ordinary Markdown. Kanban boards come from [goldmark-kanban](https://github.com/hbroek/goldmark-kanban): wrap `## column` headings with task-list cards in `<!-- kanban -->` … `<!-- /kanban -->`, or use `kanban: true` for the whole page. Pages that ask for neither render as before.
- **Spaces.** Many independent wikis on one server, each with its own SQLite file, members and an anonymous read-only switch (off for a new space until you turn it on under **Settings**).
- **Images** live in a per-space media library. Pages say `![alt](name.png)`; the server serves the picture next to the page, strips metadata, shrinks large ones and makes smaller copies on request. A page can say how big to show a picture: `![alt](name.jpg#w=400)` is 400 pixels wide and `![alt](name.jpg#h=300)` 300 high (the older `?w=400` form still works, and `#w400` is accepted as short for `#w=400`), with 60 (a thumbnail), 240, 400, 600, 800, 1200 or 1600 allowed (the **Image…** button in the editor has a size picker). A picture smaller than the size asked for is enlarged by the browser to that size. The server builds the copy when it is first asked for and keeps it in memory; the same suffix on a picture's address on another site (`![alt](https://example.com/logo.svg#w=400)`) sets the size it is shown at, nothing is fetched or resized, and the fragment is never sent to that site (handy for SVGs); without a size, large JPEG and PNG pictures show at up to 800 pixels and link to the full-size file.
- **Resized pictures from other sites.** `![alt](https://example.com/photo.jpg#w=600)` (or `#h=`, same sizes as library images) shows a smaller copy of a JPEG or PNG that lives elsewhere. The server fetches the picture when it is first asked for, keeps only the resized copy in memory for a day, and never stores the original; the picture links to the original. The **Image / PDF…** dialog has a box that writes the address with the size. Behind the scenes the page points at a signed `/_resize` address that the server makes when it shows the page, so only what someone who may edit or suggest in a space wrote into a page is ever fetched, and the server is not an open proxy. It never connects to private, loopback or link-local addresses (nor follows a redirect to one), reads at most 10 MB, and gives the other site 8 seconds. Other types (SVG, GIF, WebP, addresses without `.jpg`/`.jpeg`/`.png`) are not fetched; the size just sets how big the browser shows them. The whole feature can be switched off for the server (`-external-resize=false`, `WAKO_EXTERNAL_RESIZE=false`) and, with the server switch on, per space under **Settings** (a space administrator decides); with it off, pictures show at the size asked for but are not resized.
- **Pasting pictures.** Paste an image (a screenshot, or a picture copied from a web page) or drop a file into the editor: it is uploaded to the media library and `![pasted-20261104-153012](pasted-20261104-153012.png)` is written where the cursor was when you pasted. Screenshots, which browsers all call `image.png`, get a dated name.
- **Phone photos are shrunk in the browser.** Before a picture is uploaded (picker, paste, drop, library page), the browser scales it down to the longest side the server keeps (`-image-max-side`, 1600 px by default), halving in steps so it stays sharp, and saves a JPEG (or a PNG, for a PNG). A 12 or 48 megapixel photo leaves the phone as a few hundred KB, so it can neither be too big to upload nor have more pixels than the server will decode (`-image-max-mpix`). The camera's rotation is applied and location and date are dropped, as the server does anyway. GIFs, SVGs, PDFs, pictures that are already small enough and servers set to keep originals are sent as they are, and if the browser cannot read a picture it sends the original. HEIC files are converted where the browser can read them (Safari); iOS usually hands over JPEG anyway.
- **PDF attachments.** The media library also keeps PDF files. Upload one (or drop it on the editor), and **Image / PDF…** inserts a link, `[manual](manual.pdf)`, that opens the file. PDFs may be up to 10 MiB (`-max-pdf-mb`, `WAKO_MAX_PDF_MB`; images keep `-max-stored-mb`, 5 MiB by default), and an upload is limited by `-max-upload-mb` and are stored exactly as uploaded: wako checks the type and the size, does not read or change the file, and does not show it inside a page. They are listed, renamed and deleted in the same library page, and travel in exports and imports like images.
- **Search from Markdown browsers.** `/.well-known/bonbon.json` says `{"urlSearch": "/_search"}`, and `GET /_search?q=words` answers a Markdown page of matches (`###` headings that link to the pages, 20 a page, a "More results" link for the rest). It covers the spaces the request may read: a Bearer token also finds what its owner may see, anyone else only public spaces. Page text in the answer is escaped, so it never becomes markup.
- **History** with diffs, restore, conflict-safe saves, search, backlinks, a broken-link report and RSS feeds of changes.
- **Suggestions.** People (and programs) who may only suggest queue changes for an editor to review: a diff, accept or reject, credit to the author.
- **AI, on your terms.** Any assistant can read and edit the wiki through MCP; you can ask the wiki questions and have pages drafted. Everything an assistant writes can wait for a person to approve it.
- **PDF** through the browser: the **PDF** button opens a print view of a page and the pages below it (or the whole space) with a clean print stylesheet; choose "Save as PDF" in the print dialog.
- **Picture placement.** A button in the top bar cycles through as written, floated left, floated right (text flows round, at most half the column wide) and centred on its own line. It changes the page and the editor preview straight away, is remembered in the browser, does not touch the Markdown, and is hidden on phone-width screens, where a float leaves no room.
- **Markdown in the change feed.** Each entry of the RSS change feeds carries, besides the HTML description, a `<source:markdown>` element (the `https://source.scripting.com/` namespace, plain escaped text like Scripting News): the opening of a new page, or the diff of an edit, with pictures, attachments and page links given absolute addresses, for feed readers that render Markdown themselves.
- **A calm editor.** Hide the page tree with ☰ (Ctrl+Shift+E; remembered), a collapsed Markdown cheatsheet sits under the editor, and **Focus** (Ctrl+Shift+Enter, Esc leaves) shows nothing but the text.
- **Export and import** as a zip of ordinary `.md` files and images, plus a `wako` command-line client.

## Run it

```sh
./wakod                      # http://127.0.0.1:8888/, data in ./data
```

The first start creates the space `main` and the administrator `admin`; the generated password is printed once on stderr (or set `WAKO_ADMIN_PASSWORD`). Lost it? `wakod reset-admin-password`.

With Docker (a 22 MB image from `scratch`, running as a non-root user):

```sh
docker build -t wako .
docker run -d -p 8888:8888 -v wako-data:/data wako
docker logs <container>      # the generated admin password is in here
```

Write a page from the command line:

```sh
curl -X PUT -H "Authorization: Bearer $TOKEN" -H "Content-Type: text/markdown" \
     --data-binary @notes.md http://localhost:8888/main/notes
curl -H "Accept: text/markdown" http://localhost:8888/main/notes
```

Tokens are made on the Account page. Whether you may edit a page is announced in its response headers (`Allow`, `Accept-Patch`, `Link: rel="edit"`, `WAC-Allow`).

## Try the demo

`demo/` holds a small wiki that shows the features: text, lists, tables, to-dos, internal and external links, pictures, a PDF, front matter and macros, and kanban boards. Build a database from it and open it:

```sh
make build
deploy/seed-demo.sh                        # makes ./demo-data
./bin/wakod -data-dir ./demo-data -disable-auth
```

Then open <http://127.0.0.1:8888/demo/welcome>. `-disable-auth` makes everyone an administrator, so use it only on your own machine. The demo is also an ordinary folder of Markdown, so `wako import --create demo` loads it into any server you can sign in to.

## Settings

Every flag can also be set as an environment variable (`-data-dir` is `WAKO_DATA_DIR`, and so on). Run `wakod -h` for the list with defaults.

| Flag | Meaning |
|---|---|
| `-addr` | listen address; `unix:/path/sock` listens on a Unix socket |
| `-data-dir` | where the databases live (keep it private: it holds password hashes and the signing key) |
| `-base-path` | URL prefix when served below a path |
| `-public-url` | the address people use, for feed links behind a proxy |
| `-site-name`, `-logo` | the header title and a custom icon (the built-in one is used otherwise) |
| `-admin-user`, `-admin-password` | the administrator created on first start |
| `-disable-auth` | everyone is an administrator; for a machine you fully trust |
| `-allow-insecure` | allow login cookies over plain HTTP |
| `-allowed-host` | with `-disable-auth`: extra host names to answer to |
| `-access-token-timeout`, `-refresh-token-timeout` | sign-in lifetimes |
| `-trusted-proxies` | networks whose `X-Forwarded-*` headers are believed |
| `-tls-cert`, `-tls-key` | serve HTTPS directly |
| `-log-format`, `-log-level`, `-access-log` | `text` or `json` logs, verbosity, one line per request |
| `-external-resize` | on by default; `false` stops `/_resize` fetching pictures from other sites, whatever a space says |
| `-max-upload-mb`, `-max-stored-mb`, `-max-pdf-mb`, `-max-import-mb` | size limits: the largest upload, the largest image kept, the largest PDF kept, the largest import |
| `-image-mode`, `-image-max-side`, `-image-max-mpix` | `compact` or `original`, and picture limits |
| `-space-quota-mb` | media quota per space |
| `-revision-window`, `-keep-revisions` | how saves are merged into revisions and how many are kept |
| `-ai-provider`, `-ai-key`, `-ai-model`, `-ai-url` | the language model behind "ask" and drafts (see "Ask the wiki") |
| `-ai-max-tokens`, `-ai-timeout`, `-ai-requests-per-hour` | longest answer, how long to wait for the model, and each user's hourly allowance |

## Suggestions

Each member of a space has a role: **viewer** (read), **suggester** (read, and propose changes), **editor** (read and write) or **admin** (also members and settings). When a suggester, or a request with a *suggest-only* API token, sends `PUT`, `PATCH` or `POST` to a page, nothing is changed. The server queues it and answers `202 Accepted` with the address of the suggestion:

```sh
curl -X PUT -H "Authorization: Bearer $SUGGEST_TOKEN" -H "Content-Type: text/markdown" \
     -H "X-Wako-Note: fix the typo" --data-binary @page.md http://localhost:8888/main/notes
# 202 Accepted, Location: /main/_proposals/12
```

Editors review at `/main/_proposals` (the header shows how many are waiting): they see the diff, then accept, reject, or accept with their own text. An accepted suggestion becomes an ordinary edit credited to whoever suggested it, with a note saying who accepted it. If the page changed meanwhile, the suggestion is replayed onto the new text, and refused if it no longer fits. Suggesters see only their own suggestions; readers see none. Moving, deleting and restoring stay with editors.

Make a *suggest-only* token on the Account page for anything you want to supervise: even if its owner is an administrator, the token can only suggest. Headers announce this: a suggester's responses carry `Wako-Mode: suggest`.

## AI assistants (MCP)

wako speaks the [Model Context Protocol](https://modelcontextprotocol.io/), so Claude Code, Claude Desktop and other assistants can use the wiki directly, with an ordinary API token:

```sh
# over HTTP, to a running server
claude mcp add --transport http wako https://wiki.example.com/_mcp --header "Authorization: Bearer wako_..."
# or as a local program, over standard input and output
claude mcp add wako -e WAKO_SERVER=https://wiki.example.com -e WAKO_TOKEN=wako_... -- wako mcp
```

Tools: `list_spaces`, `list_pages`, `get_page`, `search`, `backlinks`, `broken_links`, `list_suggestions`, and for changing things `edit_page` (replace one exact piece of text), `append_to_page` and `write_page`. Writes use the page's version, so an assistant never overwrites someone's edit by accident, and each one is recorded in the history as made through MCP with that token. There is deliberately no tool to accept a suggestion or delete a page: assistants propose, people decide. The assistant can do exactly what its token can: with a suggest-only token every change waits for review.

## Ask the wiki

Set a model and the server offers **Ask** (a question answered only from pages you may read, with links to the pages it used) and **Draft a page** (describe a page; you get Markdown to read, edit and save, outside the editor). The CLI has `wako ask` and `wako draft`.

```sh
export WAKO_AI_PROVIDER=anthropic      # or openai: any OpenAI-compatible service, such as a local Ollama
export WAKO_AI_KEY=...                 # not needed for a local server
export WAKO_AI_URL=http://localhost:11434/v1   # only when it is not the provider's own address
./wakod
```

Then a server administrator switches the features on, space by space, under the space's settings, because the text of that space's pages is sent to the model's service. They are off until then. Details that matter:

- **Only what you may read.** The pages given to the model are chosen by full-text search among the spaces the asker can read. A private space never reaches someone who is not a member.
- **Nothing saves by itself.** A draft is only text. Saving it is the person's own write: an editor publishes it, a suggester queues it for review, and the history says "drafted by AI (model)".
- **Pages are data, not instructions.** Page text goes to the model marked as untrusted, the model has no tools in Ask, and answers are shown without images (so a page cannot make the answer send your question to another site).
- **Cost control.** Only signed-in users can use it; each has an hourly allowance (`-ai-requests-per-hour`); a question with no matching pages is answered without calling the model.
- **In the editor.** With the features on for the space, a box under the editor takes instructions: "fix spelling and grammar", "outline as a bulleted list", "table with 3 columns and 5 rows". Selected text is what it works on; with nothing selected, a fix covers the whole page. New content goes to the end of the page, or to the cursor (after the selection) when the instruction says "here" or "insert". The change appears in the editor only: it is not saved, **Undo** (or Ctrl+Z, straight after) takes it back, and Ctrl+/ jumps to the box. A fix that the model could not finish is never applied. It needs edit or suggest rights in the space.
- **Retrieval is keyword search**, not embeddings. It finds pages that share words with the question; it can miss a page that uses different words.

## Behind a reverse proxy

Keep wako on `127.0.0.1:8888`, let the proxy do HTTPS, and tell wako who the proxy is:

```sh
WAKO_TRUSTED_PROXIES=127.0.0.1/32 WAKO_PUBLIC_URL=https://wiki.example.com ./wakod
```

Without that, `X-Forwarded-For`, `-Proto` and `-Host` are ignored (so a visitor cannot pretend to be someone else, and the login rate limit sees the real address). Examples for nginx and Caddy are in `deploy/`. Set the proxy's upload limit above `-max-upload-mb` and `-max-import-mb`.

## Run with Docker

Build the image, run it with a volume, and put a reverse proxy in front: [`deploy/docker.md`](deploy/docker.md).

## Install as a service

`deploy/wakod.service` is a hardened systemd unit (dedicated `wako` user, state in `/var/lib/wako`, settings in `/etc/wako/wako.env`, see `deploy/wako.env.example`).

## Operating it

| Command | What it does |
|---|---|
| `wakod spaces [-data-dir D]` | which database file (`spaces/3.db`) holds which space, with page count and size |
| `wakod backup -data-dir D -to DIR` | consistent copy of every database while the server runs |
| `wakod check [-deep] [-repair]` | integrity check; `-repair` rebuilds the search index and stored paths |
| `wakod gc-assets` | remove pictures no page uses |
| `wakod forget-space SLUG` | drop a space whose file is gone |
| `wakod healthcheck` | exit 0 if the server answers (used by the Docker `HEALTHCHECK`) |
| `wakod version` | version, commit, Go version |

The server refuses to start when a space's file is missing (it would look empty), and stops gracefully on SIGTERM, finishing requests in flight.

## Security notes

Raw HTML in Markdown is not shown. Output is also sanitised, pages carry a strict Content-Security-Policy with no inline script, state-changing cross-site requests are refused, and requests for unknown host names are refused (DNS-rebinding protection). A space you cannot read answers 404, as if it did not exist. Backups and exports hold secrets; keep them private.

## Build

```sh
make build      # bin/wakod and bin/wako
make test       # vet + tests
make fuzz       # a short fuzzing round of every parser
make cross      # five platforms, both programs
make release VERSION=1.0.0   # archives and SHA256SUMS in dist/
make docker
```

Go 1.27 or newer (see `go.mod`), no C compiler needed.

## License

[MIT](LICENSE)
