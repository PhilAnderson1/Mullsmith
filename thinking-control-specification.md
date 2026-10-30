# Thinking control implementation specification

Add a thinking selector to the existing chat app. The app continues to use one endpoint and one model from its existing `llm` block. Choices come from the configured thinking profile; the endpoint definition translates a choice into JSON request fields. New profiles and new request-field mappings must be installable through YAML without recompiling the Go application.

This specification accompanies `endpoints.yaml` and `thinking-profiles.yaml`. Both YAML files contain runtime data only. This document contains the implementation rules, research, sources and deployment limitations. Research date: 29 October 2026.

## 1. Scope and design

Extend the existing single-endpoint application. Reuse its `llm` configuration, authentication, request builder, conversation storage and streaming infrastructure. Add exactly four configuration fields under `llm`; do not introduce connection registries, model registries, endpoint switching or a model selector. The catalogue files list reusable definitions, not active connections. Only the definition and profile selected in `llm` are active.

The first version uses explicit endpoint and profile selection. Automatic capability discovery, filesystem watchers, a profile editor and automatic model-name detection are not required. The model ID alone cannot establish the capabilities of a provider, quantisation or custom template.

Ship these three files:

| File | Purpose |
|---|---|
| `thinking-control-specification.md` | Instructions for the coding agent and evidence supporting the definitions |
| `endpoints.yaml` | API protocol, request route and thinking request fragments |
| `thinking-profiles.yaml` | Ordered thinking choices and their semantic values |

The supplied endpoint catalogue covers Gufo, llama.cpp, vLLM, LM Studio, Ollama and OpenRouter. LM Studio and Ollama each have separate native and OpenAI-compatible entries. These are eight definitions for six services, not eight new inference engines.

A definition describes how to express a request, not whether a server can load a model. For example, selecting a GPT-OSS profile does not add GPT-OSS inference support to Gufo. Only configure combinations supported by the actual deployment.

## 2. Additions to the existing app configuration

Extend the existing `llm` block exactly as follows. Preserve the existing fields, values, types, defaults and behaviour.

```yaml
llm:
  endpoint: "http://192.168.0.2:8080/v1"
  api_key: ""
  model: "Qwen3.8 Flash Next"
  max_context_tokens: 32768
  default_temperature: 0.7
  request_timeout: 5m
  max_tokens_response: 16384
  show_reasoning: false
  vision: true
  vision_max_image_bytes: 1MiB

  thinking_selector_enable: true
  endpoints_file: ./endpoints.yaml      # the endpoint catalogue (sole source)
  profiles_file: ./thinking-profiles.yaml # the thinking-profile catalogue (sole source)
  endpoint_type: gufo
  thinking_profile: qwen3_8
```

The five fields are:

| Field | Meaning | When omitted or empty |
|---|---|---|
| `llm.thinking_selector_enable` | Explicitly shows (`true`) or hides (`false`) the thinking selector | Legacy rule: a selector exists exactly when an `endpoint_type` (or non-`none` `thinking_profile`) is spelled out |
| `llm.endpoints_file` | Endpoint catalogue path — the only place endpoint IDs come from | A configured `endpoint_type` is a startup error |
| `llm.profiles_file` | Thinking-profile catalogue path — the only place profile IDs come from | Allowed only when the profile is `none` (the built-in no-choices entry) |
| `llm.endpoint_type` | Exact ID under `endpoints` | Retain the existing transport; no thinking selector. With the switch on, it is required |
| `llm.thinking_profile` | Exact ID under `profiles` | Use `none`; no thinking selector |

The switch set to `false` hides the selector even when the other fields are spelled out; set to `true` it requires an `endpoint_type` (a `thinking_profile` left empty behaves as `none`). A non-`none` profile requires a valid endpoint type. Unknown explicitly selected endpoint/profile IDs are configuration errors. An explicitly selected endpoint type must use a transport supported by the app, even when its profile is `none`. With all five fields omitted, the existing app configuration must continue working as before.

`llm.endpoint` remains the API base URL; `llm.model` remains the model identifier sent to that endpoint. Neither value moves to a catalogue. `endpoint_type` selects a request convention, not another server address. `thinking_profile` selects the controls appropriate to the one configured model.

Resolve relative catalogue paths against the app config file's directory, not the process working directory. Retain the existing handling of `api_key`, timeouts, context limits, output limits, temperature and vision settings.

Do not add a `default_thinking` configuration field. A new conversation starts at the built-in **Default** choice, which sends no thinking override. The user's selection is conversation state, not another global LLM setting.

`show_reasoning` continues to control whether reasoning is displayed. It must not enable or disable model thinking. With the example configuration, a user may request High thinking while `show_reasoning: false` keeps the reasoning hidden.

The supplied catalogues are external YAML files and the sole source of endpoint and profile IDs: nothing is baked into the binary beyond the built-in no-choices profile `none`, which is why `profiles_file` may be omitted exactly when the profile is `none`. An entry is exactly what its file says, so editing, adding and deleting all take effect on restart. Explicitly configured unreadable or invalid files are startup errors, as is a selection whose catalogue file is missing (`endpoint_type` without `endpoints_file`, a named `thinking_profile` without `profiles_file`). Files configured but not yet referenced are still parsed and validated, so typos fail at startup. Reload on application restart; hot reload is unnecessary.

The new catalogue schema is version 1. It replaces the earlier research-heavy `endpoints.yaml` prototype; do not attempt to parse that prototype as this schema.

## 3. Endpoint catalogue format

```yaml
schema_version: 1
endpoints:
  example:
    protocol: openai_chat
    path: /chat/completions
    requests:
      "off":
        chat_template_kwargs:
          enable_thinking: false
      "on":
        chat_template_kwargs:
          enable_thinking: true
      effort:
        chat_template_kwargs:
          enable_thinking: true
        reasoning_effort: "$effort"
      budget:
        chat_template_kwargs:
          enable_thinking: true
        reasoning_budget_tokens: "$budget_tokens"
    effort_values: [low, medium, high]
    budget_min: 1
    budget_max: 2147483647
```

Each endpoint entry has only these fields:

| Field | Type | Meaning |
|---|---|---|
| `protocol` | String | Existing or newly implemented transport adapter: `openai_chat`, `ollama_chat` or `lm_studio_chat` |
| `path` | String | Request route appended to the connection base URL |
| `requests` | Map of JSON-compatible objects | One complete thinking fragment per supported mode: `off`, `on`, `effort`, `budget` |
| `effort_values` | Optional array of strings | Endpoint wire-value allowlist; absent means the catalogue imposes no additional effort restriction |
| `budget_min` | Optional positive integer | Minimum accepted custom budget for this definition; default 1 |
| `budget_max` | Optional positive integer | Maximum custom budget for this definition; absent means no catalogue-specific ceiling |

A missing request mode means that this definition does not provide that operation. Do not invent a fallback. The catalogue deliberately excludes unverified request mappings, documentation, source URLs, display settings, history policies and capability-discovery fields.

`path` is a suffix: join `strings.TrimRight(llm.endpoint, "/")` and `path`, preserving any reverse-proxy prefix. Reuse the existing URL construction where it already does this; append the route exactly once. Do not use URL resolution that discards the base URL's path. Base URLs here include the API root:

| Definition | Example base URL | Request route |
|---|---|---|
| `gufo` / `llama_cpp` | `http://localhost:8080/v1` | `/chat/completions` |
| `vllm` | `http://localhost:8000/v1` | `/chat/completions` |
| `lm_studio` | `http://localhost:1234/v1` | `/chat/completions` |
| `lm_studio_native` | `http://localhost:1234/api/v1` | `/chat` |
| `ollama` | `http://localhost:11434/api` | `/chat` |
| `ollama_openai` | `http://localhost:11434/v1` | `/chat/completions` |
| `openrouter` | `https://openrouter.ai/api/v1` | `/chat/completions` |

### Request fragment substitution

There are exactly two reserved whole-value tokens:

- `"$effort"` becomes the selected option's `effort` string.
- `"$budget_tokens"` becomes the selected option's `budget_tokens` integer, not a numeric string.

Recursively substitute values in the selected fragment. Do not substitute inside keys or larger strings, run expressions, expand environment variables, or interpret a template language. Reject reserved tokens in the wrong request mode or without a corresponding option value. Reject any other string starting with `$` in a request fragment, to catch misspellings.

Only one request mode is selected per request. An `effort` fragment includes any enabling fields required by that endpoint, so it is not merged with the separate `on` fragment. This avoids collisions on APIs where On and effort share one field. Likewise, a budget fragment is a complete operation; never implicitly combine effort and budget.

### Why there are no automatic effort aliases

Endpoint acceptance does not imply a model implements every accepted level. Keep the model's native effort in its profile. Do not globally map `high` to `xhigh`, `medium` to `high`, or `max` to another level. Add a separately named, deployment-specific definition/profile if an administrator has verified a required mapping. The supplied Qwen3.8 profile labels its native `xhigh` option “High”; that is an explicit UI label, not an endpoint alias.

## 4. Thinking profile format

```yaml
schema_version: 1
profiles:
  example:
    options:
      - id: "off"
        label: Off
        mode: "off"
      - id: low
        label: Low
        mode: effort
        effort: low
      - id: high
        label: High
        mode: effort
        effort: xhigh
```

Each profile contains only an `options` array. Array order is UI order. Each option has:

| Field | Required | Meaning |
|---|---|---|
| `id` | Yes | Stable choice identifier, unique within the profile |
| `label` | Yes | User-facing text |
| `mode` | Yes | `off`, `on`, `effort` or `budget` |
| `effort` | Only for `effort` | Native string to pass to the endpoint fragment |
| `budget_tokens` | Only for `budget` | Positive integer cap, explicitly chosen by an administrator |

No other mode-specific fields are allowed. Reserve the ID `default` for the application's built-in choice. The `none` profile has an empty options array: it disables the UI feature and adds no request fields. It does **not** force a thinking model to stop thinking.

Do not put model IDs, endpoint names, sampling parameters, provider URLs or research notes in profiles. `llm.thinking_profile` explicitly associates the single configured `llm.model` with a profile. Two families may intentionally have identical choices but different profile IDs; this allows later changes without altering unrelated configurations.

### Built-in profile applicability

All rows below additionally offer the application's Default choice. Lists describe official released instruction/chat variants, not base checkpoints, fine-tunes or arbitrary hosted aliases.

| Profile | Models covered by the research | Choices shipped |
|---|---|---|
| `none` | Any model for which no thinking control should be shown | None |
| `qwen3_5` | Qwen3.5 0.8B, 2B, 4B, 9B, 27B, 35B-A3B, 122B-A10B and 397B-A17B | Off, On |
| `qwen3_6` | Qwen3.6-27B and Qwen3.6-35B-A3B | Off, On |
| `qwen3_8` | Qwen3.8-27B and Qwen3.8-Flash-Next | Off, Low, Medium, High; High sends `xhigh` |
| `qwen3_8_thinking_only` | Qwen3.8-2.4T-A95B | Low, Medium, High; High sends `xhigh` |
| `gpt_oss` | GPT-OSS-20B and GPT-OSS-120B | Low, Medium, High |
| `gemma4` | Gemma 4 instruction models E2B, E4B, 12B, 26B-A4B and 31B | Off, On |
| `deepseek_v4` | Original DeepSeek-V4-Flash and DeepSeek-V4-Pro | Off, High, Max |
| `deepseek_v4_0731` | DeepSeek-V4-Flash-0731 | Off, Low, High, Max |
| `glm5_3` | GLM-5.3 | Low, High, Max |

Qwen3.5-0.8B and 2B default to non-thinking in their official templates; the larger checked Qwen3.5 variants default to thinking. They nevertheless share a toggle profile because Default sends nothing. Qwen3.6 also supports a separate historical-thinking preservation control, which is not part of this selector. Qwen3.8-2.4T-A95B rejects `enable_thinking=false`; its separate profile omits Off.

GPT-OSS offers three trained effort levels, not a documented native Off mode. GLM-5.3 is thinking-only; unrecognised effort names can resolve to Max, so do not manufacture Medium. The original DeepSeek V4 and the 0731 revision use different effort sets and must remain separate.

No blanket Qwen3.7 profile is included: the research did not establish an official open-weight model/template equivalent to the checked variants. Cloud product names, newer DeepSeek V4.1 releases and other families need their own verification. Do not attach a nearby profile merely because a version number looks similar.

### Token budgets

No supplied model profile invents Low/Medium/High as token budgets. Native effort is guidance to the model; a server-enforced budget ends reasoning after a token allowance. They are different controls.

The schema supports a `budget` option so administrators can add verified budget presets without recompilation. For example, a custom profile can add an option with `id: short`, `label: Short`, `mode: budget`, and `budget_tokens: 2048`. This is an application preset, not an official model effort level. Only add it for a tested endpoint/model combination with a working reasoning parser and cap implementation.

Use positive budgets in this first version. Keep Off separate from a zero budget. Omission leaves server defaults in force; do not send `-1` as a supposedly universal unlimited value. Reserve output space for the answer: if a fixed total output cap is configured, reject a custom budget greater than or equal to it. OpenRouter can translate budgets into native effort and cannot promise an exact cap for every provider. Some providers also impose model-specific minimums beyond the endpoint catalogue's generic validation.

## 5. Available choices and UI behaviour

For the single endpoint/model configured in `llm`:

1. Look up `llm.endpoint_type` and `llm.thinking_profile` by exact ID.
2. Start with Default, labelled “Default”, ID `default`.
3. Walk profile options in order. Include an option only if the endpoint has its request mode. For effort options, also require membership in `effort_values` when that allowlist is present. For budgets, validate the endpoint range.
4. Log a clear configuration warning for options omitted by endpoint limitations. If a nonempty profile has no usable options, reject that configured combination rather than silently hiding all its controls.
5. Hide the selector for `none` or a legacy configuration without the new fields. A model that requires thinking must never acquire an Off choice through endpoint capability alone.

Default means “use server/model settings”, not “Medium”, “On”, or “automatic task analysis”. Do not display a guessed native default. Use profile labels verbatim, so the Qwen3.8 display label High correctly sends `xhigh` while GPT-OSS High sends `high`.

Add a small Thinking selector near the composer or existing generation controls; do not add a model or endpoint selector. For the supplied Gufo/Qwen3.8 configuration, show Default, Off, Low, Medium and High in that order. New and pre-existing conversations without a saved selection start at Default.

Persist the choice per conversation using the existing storage approach, with the configured endpoint URL, model ID, endpoint type and profile ID as its applicability context (or a stable fingerprint of those values, excluding credentials). This is a small conversation-state addition, not a model registry. On conversation load, restore the choice only if that context still matches and the choice remains available. Otherwise reset visibly to Default. On a stale or invalid choice submitted to the backend, return a validation error; do not silently translate it. Keep choices isolated between conversations and users.

Snapshot the selection when generation starts. Changing the selector affects the next request, not an in-flight stream. Use the same selection for every tool-call continuation within that response. Store the actual choice/profile used with response metadata if the app already records generation settings. Retry and regeneration should follow the app's existing settings semantics and show the choice that will be sent.

## 6. Request construction and conflict handling

Implement one generic request-fragment renderer, not a switch statement containing model or provider names.

1. Build a fresh normal request using the existing protocol adapter.
2. Validate the incoming choice against the backend's effective choices. Never trust a client-provided effort string or JSON fragment.
3. For Default, add no thinking fragment.
4. Otherwise, select the endpoint's request fragment by option mode, copy it, substitute its permitted token, and recursively merge it into the normal JSON request.
5. Preserve unrelated fields in nested objects. For example, setting `chat_template_kwargs.enable_thinking` must preserve another unrelated template argument.
6. Serialise once and send through the existing authenticated connection. Do not wrap custom fields in an `extra_body` JSON property; that name belongs to SDK calling conventions, not the HTTP body.

Known thinking fields already injected by the app or its raw extra-parameters configuration must be reconciled during migration. There must be one owner of these controls. A fresh request must never inherit the previous turn's effort, enabling flag or budget. Default adds no override, but cannot remove server-side defaults.

Reject conflicting static raw thinking settings when the selector is enabled, rather than letting last-writer-wins produce an unexpected setting. Validate conflicts once at configuration load where possible. The agent should inspect existing request construction for aliases including `reasoning_effort`, `reasoning`, `think`, `thinking`, `enable_thinking`, template thinking switches and budget fields. This migration audit belongs in code/config validation; it is not a new research metadata section in the endpoint file.

Do not modify temperature, top-p, total output limits, system messages, prompt suffixes or history-preservation policy as a side effect of choosing a thinking level. Do not inject `/think`, `/no_think`, `/nothink`, or hand-written effort instructions. Do not use a stop sequence to simulate a budget: it can prevent the final answer.

If the server rejects a setting, surface the error with endpoint/model/choice context. Never automatically retry with the thinking controls removed. HTTP 200 alone is not proof a control was honoured; deployment smoke tests are required.

## 7. Transport and response handling

`openai_chat` reuses the current Chat Completions adapter, including `messages`, tools, SSE and cancellation. Changing endpoint type must not reformat messages unnecessarily.

The catalogue also contains `ollama_chat` and `lm_studio_chat` definitions. These describe different native protocols; they are not interchangeable with Chat Completions. Reuse a native adapter only if the app already has it. This thinking-control feature does not require building new native transports. If a selected endpoint definition requires an unavailable protocol, reject that selection at configuration load with a clear error; merely having unused native definitions in the catalogue is valid. Never send an OpenAI request body to a native route. New native transport support can be implemented separately.

Retain the existing visible-answer renderer and thinking display. For Chat Completions, accept separate `reasoning_content` and `reasoning` fields in messages and deltas. Use one nonempty alias when both contain the same trace; do not double-append it. Process content and reasoning independently if both occur in a chunk. Ollama native uses `message.thinking`; LM Studio native has reasoning output items and corresponding stream events. OpenRouter can also carry structured `reasoning_details`.

Keep structured/raw reasoning and tool-related continuation data separate from visible answer text. Preserve opaque blocks, signatures and ordering when a protocol requires replay. Do not concatenate reasoning into `content`. Do not indiscriminately resend every historical trace: model/protocol history rules differ. Maintain the app's existing correct history policy; any separately added native adapter must implement its documented continuation rules. This change does not introduce a user-configurable preserve-thinking toggle.

Showing or hiding the reasoning panel is independent of enabling model thinking. Do not implement Off using OpenRouter `exclude`, vLLM `include_reasoning=false`, or llama.cpp `reasoning_format=none`.

## 8. Implementation and validation checklist

The coding agent should:

1. Inspect the existing `llm` configuration struct, request builder, composer controls, streaming decoder, tool loop and chat persistence before editing.
2. Add typed catalogue structs and strict YAML parsing. Reject duplicate keys, unknown structural fields, unsupported schema versions, malformed entries, unknown modes, empty labels/IDs, duplicate option IDs, invalid placeholders and nonpositive/noninteger budgets. Request fragments themselves allow arbitrary JSON-compatible field names.
3. Load the catalogues from the configured external files only (nothing embedded) and cross-reference the selection against them. Do not require users to recompile for a new JSON field convention.
4. Add the effective-choice computation and a small Thinking selector near the composer or existing generation controls. Expose only effective choices to the frontend; keep credentials and request templates server-side.
5. Add the backend choice parameter, per-conversation persistence and generic fragment renderer. Keep all tool continuations consistent with the request snapshot.
6. Integrate with the existing transport and verify its history/streaming behaviour. Reject selection of protocols the build does not implement; do not add native adapters as a prerequisite.
7. Add focused unit tests and perform the deployment checks below. Do not claim live compatibility based only on a parsed YAML file or a changed prompt.

Required automated tests:

- The user's original `llm` block loads unchanged, and omitting all four new fields preserves legacy behaviour. The extended block loads with the exact existing values and four new fields shown in section 2.
- Catalogue files are the sole source: a new ID is available, an entry is exactly what its file says, a selection without its catalogue file fails clearly, and invalid referenced IDs fail clearly.
- Qwen3.8 High sends the string `xhigh`; GPT-OSS High sends `high`; neither GLM-5.3 nor thinking-only Qwen exposes Off.
- Every supplied profile option is checked against each endpoint recipe/allowlist. Missing recipes exclude choices, with the documented warning/error behaviour.
- Default emits no thinking override. Switching High to Off or Default leaves no stale effort/budget fields.
- Deep merging preserves unrelated template arguments; booleans remain booleans and budget substitutions remain integers.
- Unsupported client choices fail before a network request. A conflicting raw override cannot silently win.
- Effort and budget never combine implicitly. A custom budget cannot consume the whole configured output allowance.
- The existing stream decoder handles thinking then content, both in one event, tool calls, cancellation and errors for supported transports. Alias fields do not duplicate text.
- A selection changed during generation does not affect its tool continuations.
- New conversations start at Default; saved selections remain isolated per conversation/user. A changed endpoint/model/profile context resets a restored selection visibly to Default.
- `show_reasoning: false` does not prevent an explicit thinking request; changing the thinking choice does not alter that display setting.
- Unused definitions for unavailable protocols do not prevent startup; selecting one produces a clear configuration error.

Representative catalogue request expectations (thinking fragment only). Native examples describe the data and do not require adding those transports to this implementation:

| Endpoint and choice | JSON added |
|---|---|
| Gufo, Qwen3.8 Off | `{"chat_template_kwargs":{"enable_thinking":false}}` |
| Gufo, Qwen3.8 High | `{"chat_template_kwargs":{"enable_thinking":true},"reasoning_effort":"xhigh"}` |
| llama.cpp, Qwen3.6 On | `{"chat_template_kwargs":{"enable_thinking":true}}` |
| vLLM, GPT-OSS Low | `{"chat_template_kwargs":{"enable_thinking":true},"reasoning_effort":"low"}` |
| Ollama native, Qwen3.5 Off | `{"think":false}` |
| Ollama OpenAI, Qwen3.5 On | `{"reasoning_effort":"medium"}`; current boolean-only metadata maps a recognised effort to enabled |
| LM Studio native, Gemma 4 On | `{"reasoning":"on"}` |
| OpenRouter, Qwen3.8 High | `{"reasoning":{"effort":"xhigh"}}` |
| Any endpoint, Default | `{}` |

Deployment checks should cover the user's Gufo/Qwen3.8 combination first, then each installed endpoint/runtime intended for release. Check an explicit Off, each native effort, Default after an earlier override, streaming, a tool continuation and a multi-turn exchange. Use server prompt/debug inspection where available and observe actual generation. A model can legitimately produce little reasoning on a simple prompt; absence of a trace is not a sufficient test by itself.

## 9. Endpoint research and intentional limitations

### Gufo

The current Chat parser accepts template `enable_thinking` and top-level `reasoning_effort`. Non-off effort enables thinking. `reasoning_budget_tokens` is implemented, with the older `thinking_budget_tokens` alias; current parser/tests were inspected. Positive request caps override the server default, while `-1` inherits it. The shipped recipe uses the current canonical spelling only. The upstream server document also contains an inconsistent old budget disclaimer; the implementation and focused test establish the current contract. UTF-8 completion can extend a cap slightly.

Sources: [server API](https://github.com/gufo-org/gufo/blob/main/docs/SERVER.md), [request parser](https://github.com/gufo-org/gufo/blob/main/src/cli/serve/openai_chat.cpp), [reasoning budget tests](https://github.com/gufo-org/gufo/blob/main/tests/functional/reasoning_budget.py).

### llama.cpp

Current `server-common.cpp` passes effort into the template and accepts template thinking enablement. The effort recipe explicitly enables thinking because effort alone need not override a server-side Off. Current request budgets use `reasoning_budget_tokens`, with an older alias. Older builds can have different budget precedence, so the supplied definition targets the inspected current implementation. Budget enforcement requires detected reasoning boundaries.

Sources: [server documentation](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md), [request translation](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/server-common.cpp), [parameter schema](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/server-schema.cpp), [DeepSeek V4 template](https://github.com/ggml-org/llama.cpp/blob/master/models/templates/deepseek-ai-DeepSeek-V4.jinja), [0731 template](https://github.com/ggml-org/llama.cpp/blob/master/models/templates/deepseek-ai-DeepSeek-V4-Flash-0731.jinja).

### vLLM

Current documentation covers top-level effort, `chat_template_kwargs.enable_thinking`, and `thinking_token_budget`. The latter needs a reasoning parser and recognised start/end boundaries. A server `reasoning-effort-map` can remap client values, so deployment configuration remains relevant. These supplied profiles use the enable_thinking convention; unrelated families with a different template key need a separately configured request definition.

Sources: [reasoning controls](https://docs.vllm.ai/en/latest/features/reasoning_outputs/), [server options](https://docs.vllm.ai/en/latest/cli/serve/), [Chat protocol values](https://docs.vllm.ai/en/v0.32.0/api/vllm/entrypoints/openai/chat_completion/protocol/), [DeepSeek V4 tokenizer adapter](https://github.com/vllm-project/vllm/blob/main/vllm/tokenizers/deepseek_v4.py).

### LM Studio

The 0.4.8 changelog confirms Chat Completions `reasoning_effort`. The public native API explicitly documents `reasoning` values `off`, `on`, `low`, `medium`, `high`, restricted by the loaded model. The supplied `lm_studio` definition therefore contains effort only: no unverified OpenAI On/Off or reasoning-token-budget recipe is shipped. The `lm_studio_native` definition describes documented toggle controls but requires a native adapter; it cannot be selected if the app only implements Chat Completions.

Native High is not proven to mean a model's `xhigh` or `max` across engines. The native definition's allowlist therefore excludes those wire strings. A Qwen3.8 profile paired with it exposes Off, Low and Medium, not a falsely equivalent High. A Qwen3.5 toggle profile with the effort-only OpenAI definition has no usable controls and fails the combination check. To expand this coverage, verify the installed engine and add an explicit endpoint override. Do not assume every llama.cpp extension is forwarded by LM Studio.

Sources: [changelog](https://lmstudio.ai/changelog/lmstudio), [Chat Completions](https://lmstudio.ai/docs/developer/openai-compat/chat-completions), [native chat](https://lmstudio.ai/docs/developer/rest/chat), [native model capabilities](https://lmstudio.ai/docs/developer/rest/list).

### Ollama

Native `think` accepts booleans or model-defined effort strings. The OpenAI adapter accepts `reasoning_effort`; `none` becomes a request to disable thinking. For current boolean-only model metadata, a recognised non-off effort becomes true; the `ollama_openai` On recipe uses `medium` for that case. Named-effort profiles select an explicit effort instead of On. A model's refusal to disable thinking is not overcome by the adapter accepting `none`.

Sources: [thinking](https://docs.ollama.com/capabilities/thinking), [native chat](https://docs.ollama.com/api/chat), [OpenAI compatibility](https://docs.ollama.com/api/openai-compatibility), [adapter implementation](https://github.com/ollama/ollama/blob/main/openai/openai.go).

### OpenRouter

Use its unified `reasoning` object. `enabled:false` requests Off, effort selects a tier, and `max_tokens` requests a reasoning allowance. Send effort or budget, not both. Providers can map these controls differently. Its model metadata may provide a future discovery enhancement, but the initial implementation uses explicitly selected profiles. Do not assume a local open-weight profile exactly describes every provider-hosted alias.

Source: [unified reasoning and per-model discovery](https://openrouter.ai/docs/guides/best-practices/reasoning-tokens).

## 10. Model research sources

Use these official cards and templates to choose the profile for a configured model. A custom template or hosted provider can change the interface; the table in section 4 is intentionally limited to the checked variants.

- **Qwen3.5:** [0.8B](https://huggingface.co/Qwen/Qwen3.5-0.8B), [2B](https://huggingface.co/Qwen/Qwen3.5-2B), [4B](https://huggingface.co/Qwen/Qwen3.5-4B), [9B](https://huggingface.co/Qwen/Qwen3.5-9B), [27B](https://huggingface.co/Qwen/Qwen3.5-27B), [35B-A3B](https://huggingface.co/Qwen/Qwen3.5-35B-A3B), [122B-A10B](https://huggingface.co/Qwen/Qwen3.5-122B-A10B), [397B-A17B](https://huggingface.co/Qwen/Qwen3.5-397B-A17B). Model cards establish the toggle; inspected 0.8B/2B and larger templates confirm different defaults without different supported controls. None documents trained effort tiers.
- **Qwen3.6:** [27B](https://huggingface.co/Qwen/Qwen3.6-27B), [35B-A3B](https://huggingface.co/Qwen/Qwen3.6-35B-A3B), [27B template](https://huggingface.co/Qwen/Qwen3.6-27B/blob/main/chat_template.jinja), [35B template](https://huggingface.co/Qwen/Qwen3.6-35B-A3B/blob/main/chat_template.jinja). The checked templates expose enablement and preservation, not native effort tiers.
- **Qwen3.8:** [27B](https://huggingface.co/Qwen/Qwen3.8-27B), [Flash-Next](https://huggingface.co/Qwen/Qwen3.8-Flash-Next), [27B template](https://huggingface.co/Qwen/Qwen3.8-27B/blob/main/chat_template.jinja), [Flash-Next template](https://huggingface.co/Qwen/Qwen3.8-Flash-Next/blob/main/chat_template.jinja). Both accept exactly `low`, `medium`, `xhigh` while thinking is enabled; their default effort is `xhigh`. The [2.4T card](https://huggingface.co/Qwen/Qwen3.8-2.4T-A95B) and [template](https://huggingface.co/Qwen/Qwen3.8-2.4T-A95B/blob/main/chat_template.jinja) establish the thinking-only exception.
- **GPT-OSS:** [20B](https://huggingface.co/openai/gpt-oss-20b), [120B](https://huggingface.co/openai/gpt-oss-120b), [20B template](https://huggingface.co/openai/gpt-oss-20b/blob/main/chat_template.jinja). Three supported effort levels; default medium. Template acceptance of arbitrary text is not evidence of additional trained levels.
- **Gemma 4:** [Google's thinking guide](https://ai.google.dev/gemma/docs/capabilities/thinking), [31B instruction card](https://huggingface.co/google/gemma-4-31B-it), [template](https://huggingface.co/google/gemma-4-31B-it/blob/main/chat_template.jinja). Boolean enablement; official template defaults to off. Use instruction variants. Google's guide also distinguishes prior-turn thought stripping from within-turn tool continuation.
- **DeepSeek V4:** [Flash](https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash), [Pro](https://huggingface.co/deepseek-ai/DeepSeek-V4-Pro), [original encoder](https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash/blob/main/encoding/encoding_dsv4.py). Non-thinking, High and Max. [Flash-0731](https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash-0731) and its [encoder](https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash-0731/blob/main/encoding/encoding_dsv4.py) add Low and retain chat/non-thinking mode. Encoder defaults and server defaults can differ; Default must remain omission.
- **GLM-5.3:** [card](https://huggingface.co/zai-org/GLM-5.3), [template](https://huggingface.co/zai-org/GLM-5.3/blob/main/chat_template.jinja), [maintainer confirmation](https://huggingface.co/zai-org/GLM-5.3/discussions/21). Thinking-only, Low/High/Max, with Max as template default. This is not a profile for all GLM versions.

## 11. Completion criteria

### Checks performed on these deliverables

The supplied YAML files passed duplicate-key, schema, placeholder and value-type checks. A local renderer constructed all 194 endpoint/profile-option combinations admitted by the supplied recipes and allowlists, including the expected LM Studio exclusions. These are request-construction checks, not 194 live compatibility tests. Twelve downloaded official Qwen, GPT-OSS and GLM templates were executed to verify the researched toggle defaults, effort values and Qwen3.8 thinking-only exception. No live inference endpoints were tested.

The implementation is complete when the existing single `llm` block accepts the four new fields, users can select the configured model's supported thinking choice, the backend emits the correct typed request fragment through the existing supported transport, and the setting survives the normal conversation lifecycle. The supplied configuration must produce Default, Off, Low, Medium and High, with High sending `xhigh`. No connection/model registry, model-switching UI or new native transport is required. Adding a profile or changing a request-field mapping must require only YAML edits and an app restart. A genuinely new API protocol can still require adapter code.

Keep the supplied YAML files minimal. Future research and compatibility notes belong here or in normal project documentation, not in runtime configuration.
