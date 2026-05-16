# notify-pipeline (webhook stage)

Two-stage pipeline that drafts a haiku via the `reasoning` capability and
posts it to a Slack/Discord/Mattermost-compatible incoming webhook. The
canonical "final stage of a pipeline announces completion" pattern.

The `notify` stage:

- Renders a JSON body (default `application/json`) from the inline `body:`
  map, with each string leaf passed through Go's `text/template` against
  the standard vamp template binding plus two webhook-only extras:
  `{{ .pipeline_name }}` and `{{ env "VAR" }}` (env-var lookup).
- POSTs the JSON to the rendered URL.
- On non-2xx, fails the stage with the status code and a 1 KiB preview of
  the response body so quota / auth failures are diagnosable.
- By default retries 5xx up to two times (3 total attempts) because
  Slack / Discord / Mattermost incoming webhooks all surface transient
  errors as 5xx; opt out per stage with `retry_on_5xx: false`.

Like the youtube stage, webhook does **not** activate a vibe profile —
it's a plain HTTP client.

## Set up the webhook URL

### Slack

1. Open <https://api.slack.com/messaging/webhooks> and **Create an app**
   in your workspace.
2. Enable **Incoming Webhooks** in the app's feature panel.
3. Click **Add New Webhook to Workspace**, pick a channel, and copy the
   generated `https://hooks.slack.com/services/T.../B.../...` URL.

### Discord

1. In your server, **Edit Channel -> Integrations -> Webhooks -> Create
   Webhook**.
2. Copy the **Webhook URL**.
3. Slack's payload shape is mostly compatible with Discord's, but Discord
   wants `content` rather than `text`. Adjust the `body:` map in
   `pipeline.yaml` if you target Discord:

   ```yaml
   body:
     content: "Pipeline {{ .pipeline_name }} done."
   ```

### Mattermost

1. **System Console -> Integrations -> Incoming Webhooks -> Add Webhook**.
2. Copy the generated URL. Mattermost accepts the same `text` field Slack
   uses, so the shipped `pipeline.yaml` works without changes.

## Drop the URL into an env var

The shipped `pipeline.yaml` reads the URL from `$VAMP_SLACK_WEBHOOK` via
the webhook `env` template func so the secret never ends up in the YAML:

```yaml
url: '{{ env "VAMP_SLACK_WEBHOOK" }}'
```

Export it before running vamp:

```
export VAMP_SLACK_WEBHOOK="https://hooks.slack.com/services/..."
```

For CI you'll typically pull the URL from your secret store and export it
in the workflow step.

## Run

```
vamp run examples/notify-pipeline/pipeline.yaml --input topic="why robots dream"
```

When the pipeline completes, `webhook_response.txt` in the run dir
contains the response body the webhook returned (usually `ok` for Slack,
empty for Discord, a JSON envelope for Mattermost).

## Customising

- **Auth header**: add `headers:` to the stage. Each header value is
  rendered as a template:

  ```yaml
  headers:
    Authorization: 'Bearer {{ env "SLACK_TOKEN" }}'
  ```

- **Custom method**: set `method: PUT` for endpoints that don't accept
  POST. Allowed values: `GET`, `POST` (default), `PUT`, `PATCH`,
  `DELETE`.

- **Complex body shapes**: when the JSON body is large or has nested
  structure, ship it as a templated file rather than an inline map:

  ```yaml
  body_template_file: ./slack-block-kit.json.tmpl
  ```

  The file's contents are rendered as a single template (the same binding
  the inline form sees) and sent as the request body verbatim — useful
  for Slack Block Kit and Discord embed payloads that don't round-trip
  through Go's JSON marshaler cleanly.

- **Disable 5xx retries**: `retry_on_5xx: false` on the stage. Or supply
  your own retry: block to override the default attempt count / backoff.
