# Model catalog generation / 模型目录生成

## Sources / 来源

- Membership: https://commandcode.ai/docs/plans/go. The initial HTML contains
  structured Next.js data with `planScope.label == "Go plan"` and an explicit
  `planScope.modelIds` list. The generator preserves these request IDs, including
  casing, namespaces, speed variants and `:free`. Display names also remain the
  official deployment names. The whole `models` array includes other plans and
  must not become the Go roster.
- Reference metadata: https://models.dev/catalog.json. Canonical records in
  `models` supply limits, modalities, reasoning/tool-call booleans and descriptions.
  Provider records supply explicit identity links and separately labeled reasoning
  control references. The two original response SHA-256 values are stored in the
  generated JSON; no npm package, CLI execution, account or inference is required.
- The page is public website data, **not a promised stable API**. Downloading and
  parsing are separate functions so a future official structured Go endpoint can
  replace it. `fetched_at` denotes the generation/fetch pass; with local snapshots
  it is the generation time, not a claim that the snapshots were just downloaded.

名单只来自官方 Go 页面内嵌的原始 ID，不依赖 Provider API 交集或 npm 解包。
页面加载前的原始 HTML 已含数据，不代表服务端必然写死或不存在内部数据源。
网站格式改变、范围字段缺失、重复 ID 或套餐字段矛盾时停止更新，不从展示文本猜名单。

## Identity matching / 身份匹配

The upstream request ID in `models.json` is never rewritten. Matching only selects reference metadata:

1. Complete canonical ID, case-insensitive; then a small manufacturer namespace
   map (`Qwen` → `alibaba`, `MiniMaxAI` → `minimax`, `z-ai` / `zai-org` → `zhipuai`).
   Unqualified complete IDs require a unique canonical basename.
2. Explicit `canonical_model_id` links from an exact full provider-offering ID or
   an exact original-provider short ID. All links must agree, and manufacturer,
   declared family, version, role, date, parameter-size and quantization checks
   must pass. Provider paths are recorded in `metadata_match_sources`.
3. Structural candidates within the same manufacturer: family, version and role
   tokens must agree. A finite class of terminal deployment labels (`fast`,
   `free`, `paid`, `highspeed`, `ultraspeed`) can differ. Omitted parameter sizes
   may be supplied only by a unique candidate. Known sizes cannot disappear or
   change. Dates, quantization, modality, `thinking`, `instruct`, `preview`, etc.
   are not discarded to improve a similarity score. An undated canonical is
   preferred over historical dated variants; a missing date cannot select a
   dated-only candidate.

Conflicting explicit links stop matching; a later heuristic cannot overrule them.
Multiple compatible sizes or dates remain ambiguous. `metadata_match`,
`metadata_match_reason`, `metadata_candidates` and `metadata_ref` make the result
inspectable. The generator neither selects the highest fuzzy-string score nor
maintains an ever-growing per-model alias table. Several Go deployments may refer
to one canonical model; they remain separate roster entries.

这是“候选发现 + 硬条件过滤 + 唯一性消歧”，不是最高字符串相似度自动认领。
例如 `4` 与 `4.1`、`flash` 与 `pro`、`27b` 与 `120b`、`fp8` 与 `int4` 不能混用。
第三方 provider 的精确 canonical 关系可以证明参考身份，但不能证明 Command Code
使用它的同一部署配置。

### Checked snapshot (2026-10-16)

The public page contained 94 total entries and 56 Go IDs. All 56 Go IDs were
retained: 54 matched canonical metadata; only `stealth/cat-jam:free` and
`stealth/glyph-cluster:free` had no disclosed canonical identity.

Examples:

- `nvidia/nemotron-3.5-super-vl` → `nvidia/nemotron-3.5-super-vl-120b-a12b`, via
  Nebius's explicit full-ID canonical link. No arbitrary parameter suffix removal.
- `poolside/laguna-s-2.1-free` → `poolside/laguna-s-2.1`, via Vercel's explicit link.
- `tencent/hy3-paid` → `tencent/hy3`, a unique structural candidate, distinct from
  `hy3-preview`. The official [v1.3.0 changelog](https://commandcode.ai/changelog)
  also identifies this as Hy3's paid deployment.
- `deepseek/deepseek-v4.1-flash-fast` → `deepseek/deepseek-v4.1-flash`, via an explicit
  provider link; `deepseek/deepseek-v4-flash-fast` → the undated V4 Flash canonical
  by structural identity. Neither changes the actual requested model ID.

## Public CPA model IDs / CPA 对外模型 ID

Registration exposes `cmdcode-go/<model_name>` in both `ID` and `Name`, taking
only the final path segment of the upstream ID and preserving its case and
variant suffixes. For example, `moonshotai/Kimi-K3` is listed as
`cmdcode-go/Kimi-K3`; `stealth/cat-jam:free` becomes `cmdcode-go/cat-jam:free`.
The original IDs remain in `models.json`, compiled definitions, and gateway
requests. Bare short names and legacy namespaced full IDs remain valid for
routing, allowlists, denylists, and reasoning suffixes. Short-name collisions
are compared case-insensitively and reject a downloaded catalog, retaining the
last good snapshot. Compiled snapshots never advertise ambiguous short names.

CPA 列表仅省略厂商路径，保留模型名大小写和变体后缀；上游请求与目录原始 ID
不变。旧三段 ID、裸短名、黑白名单及推理等级后缀继续兼容。短名忽略大小写后
发生冲突时拒绝目录更新并保留上次有效名单，避免误路由。

## Capability and fallback semantics / 能力与兜底语义

- `context` / `output` with source `modelsdev` are reference model limits, not
  independently tested Command Code deployment limits. Token counts are integers;
  no display-unit parsing or decimal/binary rounding is involved.
- A single `fallback_profile` supplies context **262144** and output **65536** when
  a matching record or an individual field is missing/zero. Each filled field has
  source `fallback`; known fields are retained. Fallback output is limited to a
  known smaller context. A known output larger than fallback context remains valid.
  CPA registration reports fallback limits as unknown (0), rather than verified
  capacities. Fallback output may guide the default request budget but never caps
  an explicitly requested output size.
- `gateway_output_limit` is a separate verified gateway hard limit. Neither public
  source establishes it, so this generator does not invent one. Explicit request
  limits are not clipped to model references or fallback values.
- `reasoning`, `vision`, `tool_call`: `true` / `false` are canonical reference facts;
  `null` means unknown. A missing source does not become a fabricated `false`.
- `reasoning_options_reference` identifies an original provider and its exact
  model ID, with `toggle`, `effort`, or `budget_tokens` options. It is `null` when
  unknown; an explicit `options: []` remains distinct. Original-provider aliases
  must identify the selected canonical and agree on options. Cross-provider
  identity evidence never copies that provider's control capabilities.
- `reasoning_efforts` is reserved for verified **Command Code** accepted levels.
  These sources do not establish that set, so generated entries use `[]` and
  `reasoning_efforts_source: "unresolved"`. It does not mean “reasoning unsupported.”
  Reference options do not trigger request rejection, effort rewriting or parameter
  injection. Runtime valid effort values use the separately tested fallback logic:
  unknown capabilities pass through common levels; known unsupported levels use
  the gateway default; `off` / `none` / `minimal` omit the effort rather than promise
  reasoning is disabled. Malformed values still fail.

本次 54 条有明确底层参数，2 条 stealth 使用统一默认配置。原厂推理控制参考覆盖
43 条：toggle 24、effort 23、budget_tokens 7，类型可重叠；其中另有 3 条明确空选项，
13 条无可信原厂参考。MiMo 的 toggle 不能解释为 Command Code 支持关闭推理，Kimi 的
原厂 effort 列表也不能冒充 Command Code 的已验证硬能力。未做真实收费调用验证。

## Update and failure behavior / 更新与失败处理

`.github/workflows/models.yml` runs at `17 */6 * * *` UTC and on manual dispatch.
It fetches only the two public sources, validates both, generates JSON and compiled
Go fallback data, runs the offline quality gate, then commits only those two outputs.
Downloads have timeouts, bounded retries, response-type and 32 MiB size checks.
Failures and large unexpected roster drift stop before replacing generated outputs.
Small normal additions are permitted; reviewed large changes require the workflow's
`allow_large_drift` input. Previous metadata is not silently carried into a new match.

Both files are validated and staged before replacement. Each `os.replace` is atomic
for one file; ordinary replacement errors roll back prior writes. **Two replacements
are not a cross-file transaction**: a process kill or storage failure can leave a mixed
pair. The Go file embeds the exact JSON SHA-256, and `--check` regenerates the expected
Go bytes to detect a mismatch. CI blocks mixed pairs before commit/build publication.
Runtime reads one complete JSON snapshot and keeps its last good catalog on failure;
it does not combine partially written JSON with a mutable Go source file.

```bash
# Download the public sources and refresh both outputs.
python3 scripts/extract-models.py --prev models.json

# Reproduce offline from complete captured HTTP responses.
python3 scripts/extract-models.py --go-page /path/to/go.html \
  --models-dev /path/to/catalog.json --prev models.json

# No network; verify the checked-in pair and run fixture tests.
python3 scripts/extract-models.py --check
python3 -m unittest discover -s scripts -p 'test_extract_models.py' -v
```

Fixture provenance is documented in `scripts/fixtures/README.md`. The filtered
models.dev fixture is intentionally incomplete and must not be used as a production
catalog snapshot.
