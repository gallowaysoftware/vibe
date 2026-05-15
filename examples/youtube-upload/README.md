# youtube-upload (YouTube Data API v3 publish stage)

End-to-end pipeline that drafts a script, voices it, renders a cover image,
muxes everything into `final.mp4`, and uploads the result to YouTube as an
unlisted video. Builds on the
[`video-pipeline`](../video-pipeline/README.md) example by appending one
`type: youtube` stage on the end.

The `publish` stage:

- Talks to Google's OAuth 2.0 token endpoint to exchange a long-lived
  `refresh_token` for a short-lived `access_token`.
- Initiates a resumable upload against the
  [YouTube Data API v3 `videos.insert` endpoint](https://developers.google.com/youtube/v3/docs/videos/insert).
- PUTs the video bytes to the returned upload URL.
- Optionally POSTs a thumbnail image to the `thumbnails/set` endpoint.
- Writes the watch URL (`https://youtube.com/watch?v=<id>`) to the configured
  output file. Downstream stages can reference the URL as
  `{{ .stages.publish.output }}`.

Like the audio and ffmpeg stages, the youtube stage **does not** activate a
vibe profile — it's a network client, not a local backend — so the
scheduler groups it on its own.

## Prerequisites

You need the prerequisites from
[`video-pipeline`](../video-pipeline/README.md) (Piper TTS, ComfyUI +
SDXL-Turbo, ffmpeg, capability mappings) **plus** the YouTube OAuth setup
below.

### 1. Create a Google Cloud project

1. Open [console.cloud.google.com](https://console.cloud.google.com) and
   create a new project (or reuse an existing one).
2. In **APIs & Services -> Library**, search for "YouTube Data API v3" and
   click **Enable**.
3. Wait a few seconds for the API to provision under your project.

### 2. Configure the OAuth consent screen

1. **APIs & Services -> OAuth consent screen**.
2. Pick **External** unless you're a Google Workspace org publishing
   internally.
3. Fill in app name, support email, developer email. The other fields can be
   blank for personal use.
4. On **Scopes**, click **Add or Remove Scopes** and select
   `https://www.googleapis.com/auth/youtube.upload`. (You can also add
   `youtube.readonly` if you want to use the same credentials for fetching
   metadata in other tooling.) Click **Update**.
5. On **Test users**, add your own Google account. While the app is in
   "Testing" mode, only test users can authorize it. That's fine for a
   single-user vamp pipeline.

### 3. Create OAuth credentials

1. **APIs & Services -> Credentials -> Create Credentials -> OAuth client
   ID**.
2. Choose **Desktop app** (simplest — no redirect URI to manage).
3. Name it something memorable (e.g. "vamp-youtube"). Click **Create**.
4. Download the JSON. It contains the `client_id` and `client_secret` you
   need.

### 4. Obtain a refresh token

The refresh token is the long-lived credential vamp uses to mint short-lived
access tokens on every run. You generate it once via the OAuth auth-code
flow. Two options:

#### Option A: OAuth Playground (easiest)

1. Open [developers.google.com/oauthplayground](https://developers.google.com/oauthplayground).
2. Click the gear icon (top right), check **Use your own OAuth credentials**,
   and paste your `client_id` and `client_secret`. Close the panel.
3. In **Step 1**, scroll to **YouTube Data API v3** and select
   `https://www.googleapis.com/auth/youtube.upload`. Click **Authorize APIs**
   and approve.
4. In **Step 2**, click **Exchange authorization code for tokens**. Copy the
   `refresh_token` field from the response panel.

#### Option B: Manual auth-code flow

1. Visit
   ```
   https://accounts.google.com/o/oauth2/v2/auth?client_id=<CLIENT_ID>&redirect_uri=urn:ietf:wg:oauth:2.0:oob&response_type=code&scope=https://www.googleapis.com/auth/youtube.upload&access_type=offline&prompt=consent
   ```
   in a browser, replacing `<CLIENT_ID>` with yours. Google shows the
   auth-code; copy it.
2. Exchange it for a refresh token:
   ```
   curl https://oauth2.googleapis.com/token \
     -d code=<AUTH_CODE> \
     -d client_id=<CLIENT_ID> \
     -d client_secret=<CLIENT_SECRET> \
     -d redirect_uri=urn:ietf:wg:oauth:2.0:oob \
     -d grant_type=authorization_code
   ```
   The response JSON contains `refresh_token`.

> **Note:** the `prompt=consent` and `access_type=offline` parameters are
> required to receive a `refresh_token`. Without them Google returns only an
> `access_token` (1-hour lifetime) — useless for an automation pipeline.

### 5. Store the credentials

Drop the three values into `~/.config/vamp/youtube-credentials.json`:

```json
{
  "client_id": "<CLIENT_ID>",
  "client_secret": "<CLIENT_SECRET>",
  "refresh_token": "<REFRESH_TOKEN>"
}
```

Lock it down:

```
chmod 600 ~/.config/vamp/youtube-credentials.json
```

Override the path per-stage with `credentials_file:` if you want it elsewhere.

#### Environment-variable alternative

For CI / secret-store-backed deploys that don't want a JSON on disk, set:

```
VAMP_YOUTUBE_CLIENT_ID=<CLIENT_ID>
VAMP_YOUTUBE_CLIENT_SECRET=<CLIENT_SECRET>
VAMP_YOUTUBE_REFRESH_TOKEN=<REFRESH_TOKEN>
```

Env vars override the JSON file field-by-field when both are present, so you
can keep `client_id` and `client_secret` in the file and override only the
`refresh_token` from your secret store.

## Run

```
vamp run examples/youtube-upload/pipeline.yaml \
  --input topic="why robots dream" \
  --input title="Why Robots Dream — vamp demo"
```

When the pipeline completes, `youtube_url.txt` in the run dir contains the
watch URL of the uploaded (unlisted) video. Bump `privacy: unlisted` to
`public` in the YAML when you're ready to publish for real.

## Quota caveats

- YouTube enforces an upload quota that's shared across all OAuth clients on
  your project. The default is 6 uploads per day for a fresh consumer
  project, raised on request. Hitting the quota surfaces as a `403
  quotaExceeded` from the upload-init endpoint; vamp surfaces the raw error
  body so you can identify it in the run log.
- The `publish` stage is **idempotent only in the YouTube sense**: each
  successful run uploads a new video resource. There's no de-dup; re-running
  a finished pipeline creates a second upload. Use vamp's `--resume` to
  re-enter an existing run dir if a transient error stopped you partway —
  the stage's `youtube_url.txt` is treated as the resume marker, so a
  previous-success won't re-upload.

## Tests vs live uploads

The executor uses an injectable HTTP client (see
`internal/vamp/youtube_executor.go`). The unit tests stub the OAuth, upload,
and thumbnail endpoints so CI never makes a real YouTube call. Smoke-testing
against the real API is left to manual local runs.
