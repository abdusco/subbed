# subbed

Watch YouTube videos with your own subtitles.

You add a video and upload subtitle files for it (SRT or VTT). When you open the video in subbed, the subtitles show on top of the player, in sync. A video can have subtitles in several languages, and you pick which one to show.

subbed is a small self-hosted app: one container, no external services except YouTube itself. Optionally, it can make the subtitles for you: it downloads a video's audio, transcribes it with an AI model and translates the result into the languages you pick (this needs an [OpenRouter](https://openrouter.ai) API key).

## Quick start

You need [Docker](https://docs.docker.com/get-docker/).

```bash
docker run -d \
  --name subbed \
  -p 3000:3000 \
  -e HOST=0.0.0.0 \
  -e DATABASE_PATH=/app/data/subbed.db \
  -e ADMIN_CREDENTIALS=admin:change-me \
  -v ./data:/app/data \
  --restart unless-stopped \
  ghcr.io/abdusco/subbed:latest
```

Then open:

- **Player:** http://localhost:3000
- **Admin:** http://localhost:3000/admin (sign in with the username and password from `ADMIN_CREDENTIALS`)

Your videos and subtitles are stored in `./data`, so they survive updates and restarts.

<details>
<summary>Docker Compose</summary>

```yaml
services:
  subbed:
    image: ghcr.io/abdusco/subbed:latest
    ports:
      - "3000:3000"
    environment:
      - HOST=0.0.0.0
      - DATABASE_PATH=/app/data/subbed.db
      - ADMIN_CREDENTIALS=admin:change-me
    volumes:
      - ./data:/app/data
    restart: unless-stopped
```

Run it with `docker compose up -d`.

</details>

To update, pull the new image and recreate the container (`docker pull ghcr.io/abdusco/subbed:latest`, then `docker compose up -d` or re-run the `docker run` command).

## Using subbed

### Add a video and its subtitles

1. Open the admin page and click **Add video**.
2. Paste the YouTube link (or just the video ID). A thumbnail preview confirms you picked the right video, and the title is filled in from YouTube. Edit it if you like; a title you typed is never overwritten.
3. On the video's card, click **Add subtitle**.
4. Choose a `.srt` or `.vtt` file, enter the language code (for example `en` or `tr`), and click **Upload subtitle**.

Shortcuts for step 4:

- **Drop** a subtitle file anywhere on the admin page, or **paste** it with Ctrl+V (Cmd+V on a Mac). You can even paste the subtitle text itself.
- If the file name ends in a language code, like `movie.tr.srt`, the language is filled in for you.

A video has one subtitle per language: uploading a language again replaces the old one.

To rename a video, click **Edit** on its card. To remove a subtitle, click the **×** on its tag. **Delete** removes the video and all of its subtitles.

### Generate subtitles with AI

Instead of uploading a file, subbed can make the subtitles from the video's audio.

1. Set `OPENROUTER_API_KEY` (see [Configuration](#configuration)) and restart.
2. Open the menu in the top bar and choose **Subtitle generation**. Here you can:
   - set the **transcription model**, any OpenRouter speech-to-text model (empty uses the default, `elevenlabs/scribe-v2`),
   - paste your **YouTube cookies**, which YouTube sometimes asks for before it lets a download through (see below),
   - see the **yt-dlp** version and update it right away.
3. On a video's card, click **Generate**. Pick the spoken language (or let the model detect it) and the languages to translate into, then click **Start**.
4. Follow the job under **Generation jobs**. Open a job to read its log; it updates by itself while the job runs.

The spoken-language subtitle is made first, then each translation. A subtitle in a language the video already has is replaced. A failed translation doesn't stop the others; the job is marked failed and its log says which one. Jobs run one at a time, and a job that was running when the server stopped is marked failed.

**Cookies.** Export them from your browser as a Netscape `cookies.txt` (for example with the "Get cookies.txt LOCALLY" extension) and paste the text in. New values replace old ones with the same domain and name, and other cookies stay. They are stored in the database and are never shown again. Use a throwaway Google account: downloading while signed in can get the account flagged. Anyone who can read your `./data` folder can read the cookies, so protect it and its backups.

**yt-dlp.** If a `yt-dlp` is in the `PATH`, subbed uses it and leaves updating to you. Otherwise it downloads the latest nightly build to `bin/yt-dlp` next to the database on first start and updates it every night at 03:00. Set `YTDLP_PATH` to keep it somewhere else.

**Cost and quality.** Transcription and translation are paid OpenRouter calls, and a long video means many of them. The result is AI text that viewers will read, so check it before relying on it.

### Watch a video

Open the player and paste a YouTube link, or go straight to it:

```
http://localhost:3000/https://www.youtube.com/watch?v=VIDEO_ID
```

Share that link with anyone who can reach your server.

Below the video you can:

- **Switch subtitles** between the languages you uploaded, or turn them off. Your choice is remembered.
- **Change the size** of the subtitle text.
- **Shift the timing** in half-second steps if the subtitles run early or late. Click the value to reset it.
- Go **fullscreen** with the subtitles still visible.

If a video has no subtitles in your library, it still plays, using YouTube's own captions when available.

### Install the admin page as an app

The admin page is a progressive web app. In Chrome, Edge or Safari use **Install** (or **Add to Home Screen**) to add it to your desktop or phone. It opens quickly and the interface loads even when you're offline, but adding videos and uploading subtitles needs a connection to your server.

### Language and appearance

Both pages follow your browser language (English or Turkish) and your system's light or dark theme. Change either from the settings button in the top bar. The choice is saved in your browser.

## Subtitle files

- **Formats:** SRT and VTT. VTT files are converted to SRT when uploaded.
- **Several lines in one subtitle** are kept as separate lines, so dialogue like `- Hello` / `- Hi there` shows on two lines.
- **Formatting:** `<i>` and `<b>` tags, and `*italic*` / `**bold**`, are supported. Other tags are removed.
- **Languages:** use short codes such as `en`, `tr`, `de` or `pt-br`.

## Configuration

Set these as environment variables.

| Variable            | Default       | What it does                                                                                               |
| ------------------- | ------------- | ---------------------------------------------------------------------------------------------------------- |
| `ADMIN_CREDENTIALS` | _(required)_  | Admin login as `username:password`.                                                                        |
| `SESSION_SECRET`    | _(random)_    | Key that signs the admin sign-in cookie. If unset, a random key is used and everyone is signed out on restart. |
| `DATABASE_PATH`     | `./subbed.db` | Where the SQLite database is stored. In Docker, use a path inside your mounted volume.                     |
| `HOST`              | `127.0.0.1`   | Network interface to listen on. Use `0.0.0.0` in Docker or to allow other devices to connect.              |
| `PORT`              | `3000`        | Port to listen on.                                                                                         |
| `OPENROUTER_API_KEY` |              | OpenRouter key for generating subtitles. Without it, the **Generate** button answers that it isn't set.    |
| `TRANSLATE_MODEL`   | `google/gemini-3.5-flash-lite` | OpenRouter chat model used for translations.                                              |
| `YTDLP_PATH`        | `<data dir>/bin/yt-dlp` | Where subbed keeps its own yt-dlp when none is in the `PATH`.                                   |
| `LISTEN_ADDR`       |               | Full address such as `0.0.0.0:8080`. Overrides `HOST` and `PORT`.                                          |
| `DEBUG`             | `0`           | Set to `1` to serve the web files from disk and log in a readable format instead of JSON. For development. |

## Keeping it safe

- **Choose a real admin password.** `admin:admin` is only for trying subbed out.
- **Use HTTPS if it's reachable from the internet.** The admin password is sent when you sign in, so put subbed behind a reverse proxy that provides HTTPS (Caddy, Traefik or nginx). The sign-in cookie is marked `Secure` when the proxy sends `X-Forwarded-Proto: https`.
- **Set `SESSION_SECRET` for sign-ins that survive restarts.** Use a long random value, for example `openssl rand -hex 32`. Changing it signs everyone out. Failed sign-ins are rate limited per client. The admin page can only be installed as an app over HTTPS, apart from `localhost`.
- **Treat the stored YouTube cookies like a password.** They let anyone who reads the database act as that Google account on YouTube. The API never sends them back.
- **The player is public.** Anyone who can reach the server can watch any video you added. Adding, changing or deleting videos and subtitles requires the admin login.

## For developers

subbed is a Go ([Echo](https://echo.labstack.com)) server with a SQLite database and a plain HTML, CSS and JavaScript frontend using [Alpine.js](https://alpinejs.dev). The frontend files in `static/` are embedded into the binary.

```bash
# run locally; web files are served from ./static, so edits show on reload
DEBUG=1 ADMIN_CREDENTIALS=admin:admin go run .

# server tests
go test ./...

# frontend unit tests (Node 20+)
node --test tests/*.mjs
```

- **Layout:** `static/index.html` and `static/player/` are the player; `static/admin.html`, `static/admin/` and `static/admin-sw.js` are the admin app. `internal/youtube` has the oEmbed client and the yt-dlp wrapper; `internal/subtitle` turns audio into SRT and translates it (it knows nothing about HTTP or the database). Running it locally needs `ffmpeg` and `ffprobe` for the transcription step.
- **Admin service worker:** it caches the admin app shell for offline start. Bump `SHELL_VERSION` in `static/admin-sw.js` whenever a file it caches changes, or users keep the old version. Open tabs then show a "new version is ready" banner.
- **Debug mode and the service worker:** with `DEBUG=1`, edits can look ignored until you bump the version, clear the site's service worker, or tick "Update on reload" in your browser's developer tools.

<details>
<summary>HTTP API</summary>

Public:

```
GET /api/video?url=https://www.youtube.com/watch?v=VIDEO_ID
```

Returns the video and all of its subtitles (with their full content):

```json
{
  "video": { "id": 1, "original_url": "VIDEO_ID", "title": "Video title" },
  "subtitles": [
    {
      "id": 1,
      "video_id": 1,
      "language": "en",
      "type": "srt",
      "content": "..."
    }
  ]
}
```

Errors are returned as `{ "error": "message" }` with a matching HTTP status.

Sign in:

- `POST /api/login`: JSON body `{ "username": "...", "password": "..." }`. Answers `204` and sets a `subbed_session` cookie, valid for 30 days.
- `POST /api/logout`: clears the cookie.

Admin (needs the session cookie; without it the API answers `401`, and `/admin` redirects to `/login`):

- `GET /api/admin/videos`: list videos with their subtitles (without content)
- `GET /api/admin/oembed?url=...`: look up a video's title through YouTube's oEmbed endpoint, as `{ "title": "...", "author": "..." }`
- `POST /api/admin/videos`: add a video, JSON body `{ "url": "...", "title": "..." }`
- `PATCH /api/admin/videos/:id`: change a video's title, JSON body `{ "title": "..." }`
- `DELETE /api/admin/videos/:id`: delete a video and its subtitles
- `POST /api/admin/subtitles`: upload a subtitle, multipart form with `video_id`, `language`, `type` (`srt` or `vtt`) and `file`
- `DELETE /api/admin/subtitles/:id`: delete a subtitle (uploading a language again replaces that language's subtitle)
- `POST /api/admin/videos/:id/generate`: queue a job, JSON body `{ "languages": ["tr", "de"], "source_language": "en" }`. `source_language` is optional (empty detects it). Answers `202` with the job; `409` if the video already has a queued or running job, `503` if `OPENROUTER_API_KEY` is not set.
- `GET /api/admin/jobs`: the 50 newest jobs, each with `status` (`queued`, `running`, `done` or `failed`), `languages`, `error` and `created_at`
- `GET /api/admin/jobs/:id/logs?after=LOG_ID`: the job and the log lines after the one with that id (`{ "job": {...}, "logs": [{ "id", "at", "level", "message" }] }`), so a page can poll
- `GET /api/admin/cookies`: how many cookies are stored and for which domains, as `{ "count": 2, "domains": ["youtube.com"] }`; never the values
- `PUT /api/admin/cookies`: JSON body `{ "cookies": "<text of a Netscape cookies.txt>" }`; upserts by domain and name
- `DELETE /api/admin/cookies`: remove all cookies
- `GET /api/admin/settings`, `PUT /api/admin/settings`: `{ "transcribe_model": "...", "transcribe_model_default": "..." }`; an empty model restores the default
- `GET /api/admin/yt-dlp`: `{ "installed": true, "version": "..." }`; `POST /api/admin/yt-dlp/update` updates it

</details>
