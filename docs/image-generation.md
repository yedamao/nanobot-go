# Image generation

nanobot-go can generate and edit images through the `generate_image` tool,
a port of nanobot's (see nanobot's `docs/image-generation.md`). It is off by
default. Generated images are saved as workspace files; the agent delivers
them with the `message` tool's `media`, so they reach Telegram (`sendPhoto`),
WeChat (CDN upload) and the WebUI like any other attachment.

## Quick setup

Add the provider's key and enable the tool in `~/.nanobot-go/config.json`,
then restart the gateway (there is no live reload or WebUI toggle yet):

```json
{
  "providers": {
    "openrouter": { "apiKey": "sk-or-..." }
  },
  "tools": {
    "imageGeneration": {
      "enabled": true,
      "provider": "openrouter",
      "model": "openai/gpt-5.4-image-2"
    }
  }
}
```

For OpenAI's Images API instead:

```json
{
  "providers": { "openai": { "apiKey": "sk-..." } },
  "tools": {
    "imageGeneration": { "enabled": true, "provider": "openai", "model": "gpt-image-1" }
  }
}
```

Keys may also come from `NANOBOT_GO_OPENROUTER_API_KEY` /
`NANOBOT_GO_OPENAI_API_KEY`, and `providers.<name>.apiBase` (or
`NANOBOT_GO_<NAME>_API_BASE`) points the client at another endpoint. An
OpenAI-compatible Images API server (nanobot's `custom` provider) works as
`provider: "openai"` with `providers.openai.apiBase` set.

Then ask for an image in chat: "draw a lighthouse at dusk and send it to
me".

## Configuration reference

| Option | Default | Description |
|--------|---------|-------------|
| `tools.imageGeneration.enabled` | `false` | Register the `generate_image` tool |
| `tools.imageGeneration.provider` | `"openrouter"` | `openrouter` or `openai` |
| `tools.imageGeneration.model` | `"openai/gpt-5.4-image-2"` | Provider model name (`openai/` is stripped for OpenAI) |
| `tools.imageGeneration.defaultAspectRatio` | `"1:1"` | Used when a call names none |
| `tools.imageGeneration.defaultImageSize` | `"1K"` | Size hint, e.g. `1K` or `1024x1024` |
| `tools.imageGeneration.maxImagesPerTurn` | `4` | Largest `count` one call accepts (1–8) |
| `tools.imageGeneration.saveDir` | `"generated"` | Relative directory under `<workspace>/media/` |
| `tools.imageGeneration.extraHeaders` | none | Extra request headers |
| `tools.imageGeneration.extraBody` | none | Extra request body keys (each replaces the whole value; `null` removes a default such as `response_format`) |

## How it works

The tool takes `prompt` (required), `reference_images` (workspace image
paths, including earlier artifacts, for edits), `aspect_ratio`,
`image_size` and `count`. Each image is saved as

```text
<workspace>/media/<saveDir>/<YYYY-MM-DD>/img_<12 hex>.<png|jpg|webp|gif>
<workspace>/media/<saveDir>/<YYYY-MM-DD>/img_<12 hex>.json   (prompt, model, provider, source images)
```

with mode 0600, and the tool answers with JSON `{"artifacts": [...],
"next_step": "..."}` telling the model to call `message` with the artifact
paths in `media`. Like nanobot, images are **not** attached to the final
reply automatically.

- **OpenRouter** calls `POST {apiBase}/chat/completions` with
  `modalities: ["image", "text"]` and `image_config` (`aspect_ratio`,
  `image_size`); reference images go in as data-URL `image_url` parts.
- **OpenAI** calls `POST {apiBase}/images/generations` (`size` from
  `image_size` or the aspect ratio, `response_format: b64_json` for DALL·E),
  or `/images/edits` (multipart) with reference images, which only GPT Image
  models accept. URL results are downloaded under the SSRF policy
  (`tools.ssrfWhitelist`), at most 32 MiB.

Reference images must be existing PNG, JPEG, WebP or GIF files inside the
workspace, even when `restrictToWorkspace` is off: they are uploaded to the
provider.

## Not ported

nanobot's other image providers (`openai_codex`, `custom` named
connections, `aihubmix`, `minimax`, `gemini`, `ollama`, `stepfun`, `zhipu`,
`modelscope`), the WebUI Settings → Capabilities toggle with hot reload,
and per-provider `extraHeaders`/`extraBody`/proxy settings.
