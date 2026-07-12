package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// youtubeExecutor implements StageExecutor for type: youtube stages. It uploads
// a video file (typically the output of an upstream ffmpeg stage) to YouTube
// via the YouTube Data API v3
// (https://developers.google.com/youtube/v3/docs/videos/insert) over plain
// HTTPS. We deliberately do NOT pull in google.golang.org/api: the protocol is
// small enough (refresh-token exchange + resumable upload init + bytes PUT +
// optional thumbnail POST) that owning the HTTP plumbing here keeps the
// dependency surface unchanged and lets tests stub every call through a
// shared httpDoer.
//
// Output: writes "https://youtube.com/watch?v=<id>" to <RunDir>/<output> and
// returns the same URL as StageOutput.Text so downstream stages can reference
// `.stages.<id>.output`.
type youtubeExecutor struct {
	// doer is the injectable HTTP transport. Tests stub this so they can
	// record each request (token exchange, resumable init, bytes upload,
	// optional thumbnail) without making real YouTube calls. nil means use
	// http.DefaultClient.
	doer httpDoer
	// credsLoader, when non-nil, overrides the on-disk credentials JSON read.
	// Tests use this to avoid writing a file per case.
	credsLoader func(path string) (youtubeCredentials, error)
}

// httpDoer abstracts the single method we need from *http.Client so tests can
// stub every outbound call. Production uses http.DefaultClient.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Compile-time guarantee that youtubeExecutor satisfies StageExecutor.
var _ StageExecutor = (*youtubeExecutor)(nil)

// defaultYouTubeCredsPath is the per-user default location of the OAuth
// refresh-token credentials JSON when a stage doesn't set credentials_file.
const defaultYouTubeCredsPath = "~/.config/vamp/youtube-credentials.json"

// defaultYouTubeCategoryID is YouTube's category id for "People & Blogs",
// which is the most generic catch-all category for short-form AI-generated
// content. Users can override per stage.
const defaultYouTubeCategoryID = "22"

// youtubeCredentials is the on-disk OAuth refresh-token bundle. Users obtain
// these through the Google Cloud Console + a one-time auth-code exchange (the
// example README documents the flow). The JSON shape is intentionally minimal
// — we don't store the access token because it's short-lived and re-derived
// on every run from the refresh token.
//
// Two extra fields beyond client_id/client_secret/refresh_token let the YAML
// reference environment variables for any of the three values (handy for
// running on machines where the secret store rather than a JSON file owns the
// credentials):
//
// Environment-variable override: if the env vars VAMP_YOUTUBE_CLIENT_ID /
// VAMP_YOUTUBE_CLIENT_SECRET / VAMP_YOUTUBE_REFRESH_TOKEN are all set,
// credentials_file is not required and the executor uses the env values
// directly. Mixed mode (some env, some file) is supported: env wins when
// present, the file fills in the rest.
type youtubeCredentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

// Environment variable names for the env-based credential override. The
// triplet maps 1:1 to youtubeCredentials so a user who keeps secrets out of
// disk-rooted JSON can still drive an upload (e.g. CI with secret store).
const (
	envYouTubeClientID     = "VAMP_YOUTUBE_CLIENT_ID"
	envYouTubeClientSecret = "VAMP_YOUTUBE_CLIENT_SECRET"
	envYouTubeRefreshToken = "VAMP_YOUTUBE_REFRESH_TOKEN"
)

// Execute runs one YouTube upload. We render the stage's templated fields
// (video / title / description / thumbnail), exchange the refresh token for an
// access token, kick off a resumable upload, PUT the video bytes, optionally
// POST a thumbnail, and write the resulting watch URL to the run dir.
func (y *youtubeExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, errors.New("youtube: missing stage")
	}

	// Foreach binding goes into every template render so users can fan out
	// uploads (e.g. one per episode in a series).
	var extra map[string]any
	if st.Foreach != nil {
		extra = map[string]any{st.Foreach.Var: in.Item, "i": in.ItemIdx}
	}

	video, err := renderTemplate(st.ID+":video", st.Video, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render video path: %w", st.ID, err)
	}
	title, err := renderTemplate(st.ID+":title", st.Title, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render title: %w", st.ID, err)
	}
	description, err := renderTemplate(st.ID+":description", st.Description, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render description: %w", st.ID, err)
	}
	var thumbnail string
	if st.Thumbnail != "" {
		thumbnail, err = renderTemplate(st.ID+":thumbnail", st.Thumbnail, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render thumbnail path: %w", st.ID, err)
		}
	}

	// Resolve the video path: relative paths are rooted at the run dir so
	// upstream `output: final.mp4` references resolve without the user
	// templating in `{{.runDir}}`.
	videoPath := video
	if !filepath.IsAbs(videoPath) {
		videoPath = filepath.Join(in.RunDir, videoPath)
	}
	videoFile, err := os.Open(videoPath)
	if err != nil {
		return nil, fmt.Errorf("stage %s: open video %s: %w", st.ID, videoPath, err)
	}
	defer videoFile.Close()
	videoInfo, err := videoFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("stage %s: stat video %s: %w", st.ID, videoPath, err)
	}
	videoSize := videoInfo.Size()

	var thumbPath string
	if thumbnail != "" {
		thumbPath = thumbnail
		if !filepath.IsAbs(thumbPath) {
			thumbPath = filepath.Join(in.RunDir, thumbPath)
		}
	}

	creds, err := y.loadCredentials(st.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("stage %s: %w", st.ID, err)
	}

	privacy := st.Privacy
	if privacy == "" {
		privacy = "private"
	}
	categoryID := st.CategoryID
	if categoryID == "" {
		categoryID = defaultYouTubeCategoryID
	}

	doer := y.doer
	if doer == nil {
		doer = http.DefaultClient
	}

	if in.Log != nil {
		fmt.Fprintf(in.Log, "youtube: exchanging refresh token\n")
	}
	accessToken, err := exchangeRefreshToken(ctx, doer, creds)
	if err != nil {
		return nil, fmt.Errorf("stage %s: oauth: %w", st.ID, err)
	}

	if in.Log != nil {
		fmt.Fprintf(in.Log, "youtube: initiating resumable upload (%d byte(s))\n", videoSize)
	}
	uploadURL, err := initiateResumableUpload(ctx, doer, accessToken, youtubeVideoMetadata{
		Title:       title,
		Description: description,
		Tags:        st.Tags,
		Privacy:     privacy,
		CategoryID:  categoryID,
	}, videoSize)
	if err != nil {
		return nil, fmt.Errorf("stage %s: init upload: %w", st.ID, err)
	}

	if in.Log != nil {
		fmt.Fprintf(in.Log, "youtube: uploading video bytes\n")
	}
	videoID, err := uploadVideoBytes(ctx, doer, uploadURL, videoFile, videoSize)
	if err != nil {
		return nil, fmt.Errorf("stage %s: upload bytes: %w", st.ID, err)
	}

	if thumbPath != "" {
		thumbFile, err := os.Open(thumbPath)
		if err != nil {
			return nil, fmt.Errorf("stage %s: open thumbnail %s: %w", st.ID, thumbPath, err)
		}
		thumbInfo, err := thumbFile.Stat()
		if err != nil {
			thumbFile.Close()
			return nil, fmt.Errorf("stage %s: stat thumbnail %s: %w", st.ID, thumbPath, err)
		}
		if in.Log != nil {
			fmt.Fprintf(in.Log, "youtube: uploading thumbnail (%d byte(s))\n", thumbInfo.Size())
		}
		err = uploadThumbnail(ctx, doer, accessToken, videoID, thumbFile, thumbInfo.Size())
		thumbFile.Close()
		if err != nil {
			return nil, fmt.Errorf("stage %s: thumbnail: %w", st.ID, err)
		}
	}

	watchURL := fmt.Sprintf("https://youtube.com/watch?v=%s", videoID)
	if in.Log != nil {
		fmt.Fprintf(in.Log, "youtube: uploaded %s\n", watchURL)
	}
	return &StageOutput{Text: watchURL}, nil
}

// loadCredentials resolves the OAuth credentials from the configured source.
// Resolution order (env wins individually so partial overrides work):
//  1. Read the credentials JSON file (configured path, default
//     ~/.config/vamp/youtube-credentials.json) — if it exists; missing file is
//     not an error when env vars supply every field.
//  2. Overlay env-var values (VAMP_YOUTUBE_CLIENT_ID / _CLIENT_SECRET /
//     _REFRESH_TOKEN). Each env var that is set replaces the corresponding
//     field from the file.
//  3. Reject the result if any of the three required fields is still empty,
//     with a hint listing where to look.
func (y *youtubeExecutor) loadCredentials(path string) (youtubeCredentials, error) {
	var creds youtubeCredentials
	resolved := path
	if resolved == "" {
		resolved = defaultYouTubeCredsPath
	}
	resolved = expandTilde(resolved)

	loader := y.credsLoader
	if loader == nil {
		loader = readCredentialsFromDisk
	}

	// Try to load the file. Missing is not fatal — env vars may cover the
	// gap. Other I/O errors do propagate so silent corrupted-file failures
	// don't surface as a generic "refresh_token missing" downstream.
	fileCreds, err := loader(resolved)
	switch {
	case err == nil:
		creds = fileCreds
	case errors.Is(err, os.ErrNotExist):
		// fall through: env vars may supply the values
	default:
		return creds, fmt.Errorf("read credentials %s: %w", resolved, err)
	}

	if v := os.Getenv(envYouTubeClientID); v != "" {
		creds.ClientID = v
	}
	if v := os.Getenv(envYouTubeClientSecret); v != "" {
		creds.ClientSecret = v
	}
	if v := os.Getenv(envYouTubeRefreshToken); v != "" {
		creds.RefreshToken = v
	}

	var missing []string
	if creds.ClientID == "" {
		missing = append(missing, "client_id")
	}
	if creds.ClientSecret == "" {
		missing = append(missing, "client_secret")
	}
	if creds.RefreshToken == "" {
		missing = append(missing, "refresh_token")
	}
	if len(missing) > 0 {
		return creds, fmt.Errorf("youtube credentials incomplete (missing %s); populate %s or set %s/%s/%s environment variables",
			strings.Join(missing, ", "),
			resolved,
			envYouTubeClientID, envYouTubeClientSecret, envYouTubeRefreshToken)
	}
	return creds, nil
}

// readCredentialsFromDisk is the production credsLoader. Parses the JSON file
// at path into a youtubeCredentials. A missing file returns os.ErrNotExist so
// loadCredentials can treat that case as "env vars must supply everything"
// rather than a hard error.
func readCredentialsFromDisk(path string) (youtubeCredentials, error) {
	var creds youtubeCredentials
	data, err := os.ReadFile(path)
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return creds, fmt.Errorf("parse credentials JSON: %w", err)
	}
	return creds, nil
}

// exchangeRefreshToken trades a long-lived refresh_token for a short-lived
// access_token by calling Google's OAuth2 token endpoint. Returns the
// access_token bearer string on success.
//
// The endpoint accepts application/x-www-form-urlencoded; we use that rather
// than JSON because that's what Google's own examples use and the response
// shape is identical.
func exchangeRefreshToken(ctx context.Context, doer httpDoer, creds youtubeCredentials) (string, error) {
	form := url.Values{}
	form.Set("client_id", creds.ClientID)
	form.Set("client_secret", creds.ClientSecret)
	form.Set("refresh_token", creds.RefreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		// Surface Google's own error string when present so a misconfigured
		// refresh token surfaces as "invalid_grant" instead of "401".
		return "", fmt.Errorf("token exchange returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", errors.New("token exchange response missing access_token")
	}
	return out.AccessToken, nil
}

// youtubeVideoMetadata is the small subset of YouTube's video resource we set
// on insert. Marshalled as the wire shape Google expects (snippet + status
// nested under separate keys).
type youtubeVideoMetadata struct {
	Title       string
	Description string
	Tags        []string
	Privacy     string
	CategoryID  string
}

// MarshalJSON renders the metadata into the snippet/status nested shape the
// YouTube Data API expects on POST. Hand-rolled to keep the wire shape close
// to the docs without inventing extra typed structs.
func (m youtubeVideoMetadata) MarshalJSON() ([]byte, error) {
	snippet := map[string]any{
		"title":       m.Title,
		"description": m.Description,
		"categoryId":  m.CategoryID,
	}
	if len(m.Tags) > 0 {
		snippet["tags"] = m.Tags
	}
	body := map[string]any{
		"snippet": snippet,
		"status": map[string]any{
			"privacyStatus": m.Privacy,
		},
	}
	return json.Marshal(body)
}

// initiateResumableUpload posts the metadata to the resumable-upload init
// endpoint and returns the Location header (the per-upload URL to PUT the
// bytes to). On non-2xx it surfaces the response body so YouTube quota /
// permission errors are diagnosable.
func initiateResumableUpload(ctx context.Context, doer httpDoer, accessToken string, meta youtubeVideoMetadata, sizeBytes int64) (string, error) {
	body, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.googleapis.com/upload/youtube/v3/videos?uploadType=resumable&part=snippet,status", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build upload init request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	// X-Upload-Content-* hints let YouTube reserve quota up front. They are
	// optional per the docs but make resume / retry behaviour more
	// deterministic; we set them at Phase 1 cost zero.
	req.Header.Set("X-Upload-Content-Type", "video/*")
	if sizeBytes > 0 {
		req.Header.Set("X-Upload-Content-Length", fmt.Sprintf("%d", sizeBytes))
	}

	resp, err := doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload init: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload init returned %s: %s", resp.Status, strings.TrimSpace(string(errBody)))
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("upload init response missing Location header")
	}
	return loc, nil
}

// uploadVideoBytes PUTs the video bytes to the resumable upload URL returned
// by initiateResumableUpload. Phase 1 does a single PUT (no real chunked
// resume); a 200/201 response carries the JSON video resource with the
// assigned id. 308 Resume Incomplete is treated as a transient failure
// surface — Phase 1 fails clearly rather than silently looping, so the user
// sees the problem instead of an indefinite hang.
func uploadVideoBytes(ctx context.Context, doer httpDoer, uploadURL string, src io.Reader, size int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, src)
	if err != nil {
		return "", fmt.Errorf("build upload PUT: %w", err)
	}
	req.Header.Set("Content-Type", "video/*")
	req.ContentLength = size

	resp, err := doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload PUT: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", fmt.Errorf("parse upload response: %w", err)
		}
		if out.ID == "" {
			return "", errors.New("upload response missing id")
		}
		return out.ID, nil
	case 308:
		// Resume Incomplete. Phase 1 doesn't drive chunked resume — surface
		// the partial state clearly so the user retries the whole stage.
		return "", fmt.Errorf("upload incomplete (HTTP 308); resumable chunking is not implemented in Phase 1, retry the stage")
	default:
		return "", fmt.Errorf("upload PUT returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
}

// uploadThumbnail POSTs an image to YouTube's thumbnails/set endpoint to
// replace the auto-generated still. The image bytes are sent as the raw body
// (the docs accept image/jpeg or image/png; the executor doesn't sniff because
// YouTube does that itself and rejects mismatches with a clear error). Failure
// returns the response body so quota / permission errors are diagnosable.
func uploadThumbnail(ctx context.Context, doer httpDoer, accessToken, videoID string, body io.Reader, size int64) error {
	endpoint := fmt.Sprintf("https://www.googleapis.com/upload/youtube/v3/thumbnails/set?videoId=%s", url.QueryEscape(videoID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("build thumbnail request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	// Let YouTube sniff the actual format; image/* is accepted as a generic
	// hint by the docs.
	req.Header.Set("Content-Type", "image/*")
	req.ContentLength = size

	resp, err := doer.Do(req)
	if err != nil {
		return fmt.Errorf("thumbnail upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("thumbnail upload returned %s: %s", resp.Status, strings.TrimSpace(string(errBody)))
	}
	return nil
}
