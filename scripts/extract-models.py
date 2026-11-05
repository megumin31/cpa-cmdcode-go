#!/usr/bin/env python3
"""Build the Go roster from the public Go plan page and models.dev catalog JSON.

Only planScope.modelIds grants membership. Other sites' capabilities are reference
metadata, never proof of Command Code gateway limits or accepted reasoning values.
Python 3.11+; no package execution or inference requests.
"""
import argparse
import datetime
import hashlib
from html.parser import HTMLParser
from decimal import Decimal
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import time
import urllib.error
import urllib.request

GO_URL = "https://commandcode.ai/docs/plans/go"
MODELS_DEV_URL = "https://models.dev/catalog.json"
SOURCE = "command-code Go plan page"
MAX_SOURCE_BYTES = 32 * 1024 * 1024
MAX_TOKENS = (1 << 63) - 1
FALLBACK_PROFILE = {"context": 262144, "output": 65536}
ORIGIN_PROVIDERS = {"meituan": "longcat", "tencent": "tencent-tokenhub"}
# Manufacturer namespaces, not a list of individual model substitutions.
NAMESPACE_ALIASES = {"qwen": "alibaba", "minimaxai": "minimax",
                     "zai-org": "zhipuai", "z-ai": "zhipuai"}
# A finite semantic category, not per-model aliases. Remove terminal deployment
# labels only for identity candidates, never from request IDs. Versions, sizes,
# context labels, quantizations and thinking/instruct variants remain significant.
DEPLOYMENT_WORDS = {"fast", "free", "paid", "highspeed", "ultraspeed"}
ID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:/-]*\Z")


def fail(message):
    raise ValueError(message)


def unique_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            fail(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def parse_json(text):
    return json.loads(text, object_pairs_hook=unique_pairs,
                      parse_constant=lambda value: fail(f"invalid JSON number: {value}"))


def valid_ids(ids, label):
    if not isinstance(ids, list) or any(not isinstance(x, str) or not ID_PATTERN.fullmatch(x) for x in ids):
        fail(f"{label}: invalid model IDs")
    if len({x.casefold() for x in ids}) != len(ids):
        fail(f"{label}: duplicate model IDs (case insensitive)")


class ScriptParser(HTMLParser):
    """Collect script text without evaluating JavaScript or fetching resources."""
    def __init__(self):
        super().__init__(convert_charrefs=False)
        self.scripts = []
        self.parts = None

    def handle_starttag(self, tag, attrs):
        if tag == "script":
            self.parts = []

    def handle_data(self, data):
        if self.parts is not None:
            self.parts.append(data)

    def handle_endtag(self, tag):
        if tag == "script" and self.parts is not None:
            self.scripts.append("".join(self.parts))
            self.parts = None


def parse_go_page(text):
    parser = ScriptParser()
    parser.feed(text)
    chunks = []
    for script in parser.scripts:
        if not script.lstrip().startswith("self.__next_f.push("):
            continue
        match = re.fullmatch(r"\s*self\.__next_f\.push\((\[.*\])\);?\s*", script, re.S)
        if not match:
            fail("Go page: unrecognized Next.js data wrapper")
        value = parse_json(match[1])
        if isinstance(value, list) and len(value) == 2 and value[0] == 1:
            if not isinstance(value[1], str):
                fail("Go page: invalid text data chunk")
            chunks.append(value[1])
    # RSC records may be split across script elements. Parse JSON, never eval().
    candidates = []

    def visit(value):
        if isinstance(value, dict):
            scope = value.get("planScope")
            if isinstance(scope, dict) and scope.get("label") == "Go plan":
                candidates.append(value)
            for child in value.values():
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)

    for line in "".join(chunks).splitlines():
        match = re.match(r"^[0-9a-f]+:([\[{].*)$", line)
        if match:
            visit(parse_json(match[1]))
    if len(candidates) != 1:
        fail(f"Go page: expected one Go planScope, found {len(candidates)}")
    props = candidates[0]
    ids = props["planScope"].get("modelIds")
    valid_ids(ids, "Go planScope")
    if not 10 <= len(ids) <= 300:
        fail(f"Go count outside bounds: {len(ids)}")
    models = props.get("models")
    if not isinstance(models, list) or any(not isinstance(m, dict) for m in models):
        fail("Go page: missing model records")
    valid_ids([m.get("id") for m in models], "Go page models")
    rows = {m["id"]: m for m in models}
    missing = set(ids) - rows.keys()
    if missing:
        fail(f"Go page: planScope IDs absent from model records: {sorted(missing)}")
    # Independent page field detects accidental selection of an all-plans table.
    declared = {m["id"] for m in models if m.get("minPlanName") == "Go"}
    if set(ids) != declared:
        fail(f"Go page: planScope/minPlanName disagree: {sorted(set(ids) ^ declared)}")
    result = []
    for mid in ids:
        name = rows[mid].get("name")
        if not isinstance(name, str) or not name.strip():
            fail(f"Go page: invalid name for {mid}")
        result.append({"id": mid, "display": name})
    return result, len(models)


def index_casefold(mapping, label):
    if not isinstance(mapping, dict) or not mapping:
        fail(f"{label}: expected a nonempty object")
    index = {}
    for key, value in mapping.items():
        if not isinstance(key, str) or not key or not isinstance(value, dict):
            fail(f"{label}: invalid record")
        if key.casefold() in index:
            fail(f"{label}: duplicate case-insensitive ID: {key}")
        if value.get("id") != key:
            fail(f"{label}: key/id mismatch for {key}")
        if "canonical_model_id" in value and (not isinstance(value["canonical_model_id"], str) or not value["canonical_model_id"]):
            fail(f"{label}: invalid canonical_model_id for {key}")
        index[key.casefold()] = (key, value)
    return index


def load_models_dev(text):
    doc = parse_json(text)
    if not isinstance(doc, dict) or not isinstance(doc.get("providers"), dict):
        fail("models.dev: expected catalog {models, providers}")
    models = index_casefold(doc.get("models"), "models.dev models")
    providers = {}
    for key, provider in doc["providers"].items():
        if not isinstance(provider, dict) or provider.get("id") != key:
            fail(f"models.dev: invalid provider {key}")
        offerings = provider.get("models")
        if not isinstance(offerings, dict):
            fail(f"models.dev: invalid provider models {key}")
        if key.casefold() in providers:
            fail(f"models.dev: duplicate case-insensitive provider: {key}")
        providers[key.casefold()] = index_casefold(offerings, f"provider {key}") if offerings else {}
    return models, providers


def origin_id(mid):
    if "/" not in mid:
        return mid.casefold()
    namespace, short = mid.casefold().split("/", 1)
    return NAMESPACE_ALIASES.get(namespace, namespace) + "/" + short


def model_identity(mid):
    """Separate conservative identity tokens; never use a similarity score."""
    normalized = origin_id(mid)
    namespace, _, short = normalized.rpartition("/")
    parts = re.split(r"[-:]", short or normalized)
    deployment = []
    while parts and parts[-1] in DEPLOYMENT_WORDS:
        deployment.insert(0, parts.pop())
    sizes, quantization, core, dates = {}, [], [], []
    for part in parts:
        size = re.fullmatch(r"(a?)(\d+(?:\.\d+)?)([bt])", part)
        if size:
            role = "active" if size[1] else "total"
            value = str((Decimal(size[2]) * (1000 if size[3] == "t" else 1)).normalize())
            sizes.setdefault(role, []).append(value)
        elif re.fullmatch(r"(?:fp|bf|int)\d+|q\d+(?:_[a-z0-9]+)*", part):
            quantization.append(part)
        elif re.fullmatch(r"(?:20\d{6}|[01]\d[0-3]\d)", part):
            dates.append(part)
        else:
            core.append(re.sub(r"^v(?=\d)", "", part))
    tokens = re.findall(r"[a-z]+|\d+(?:\.\d+)*", "-".join(core))
    return {"namespace": namespace, "roles": tuple(x for x in tokens if x[0].isalpha()),
            "versions": tuple(x for x in tokens if x[0].isdigit()),
            "sizes": sizes, "quantization": tuple(quantization),
            "dates": tuple(dates), "deployment": tuple(deployment)}


def identity_conflict(query, target):
    a, b = model_identity(query), model_identity(target)
    if a["namespace"] and a["namespace"] != b["namespace"]:
        return "manufacturer_conflict"
    if a["roles"] and b["roles"] and a["roles"][0] != b["roles"][0]:
        return "family_conflict"
    variants = {"flash", "pro", "max", "plus", "mini", "nano", "ultra", "super",
                "vl", "vision", "omni", "audio", "video", "text", "thinking", "instruct", "code", "preview"}
    av, bv = set(a["roles"]) & variants, set(b["roles"]) & variants
    if av and av != bv:
        return "variant_conflict"
    if a["versions"] != b["versions"]:
        return "version_conflict"
    for role in a["sizes"]:
        if a["sizes"][role] != b["sizes"].get(role):
            return "parameter_size_conflict"
    modalities = {"vl", "vision", "omni", "audio", "video", "text", "thinking", "instruct"}
    if (set(a["roles"]) & modalities) != (set(b["roles"]) & modalities):
        return "modality_or_reasoning_variant_conflict"
    for key in ("quantization", "dates"):
        if a[key] != b[key]:
            return key + "_conflict"
    return ""


def explicit_canonical_links(models, providers, mid):
    normalized = origin_id(mid)
    namespace, _, short = normalized.rpartition("/")
    origin = ORIGIN_PROVIDERS.get(namespace, namespace)
    links = []
    for provider_id, offerings in providers.items():
        for key, (offering_id, offering) in offerings.items():
            exact_full = "/" in mid and key in {mid.casefold(), normalized}
            exact_origin = namespace and provider_id == origin and key == short
            if not (exact_full or exact_origin) or "canonical_model_id" not in offering:
                continue
            target = offering["canonical_model_id"]
            links.append((target, "providers/" + provider_id + "/models/" + offering_id))
    return links


def resolve_metadata(models, providers, mid):
    def result(target=None, method="unresolved", sources=(), candidates=(), reason=""):
        return {"canonical": target, "method": method, "sources": list(sources),
                "candidates": list(candidates), "reason": reason}

    key = mid.casefold()
    if key in models:
        return result(models[key][0], "exact")
    normalized = origin_id(mid)
    if normalized in models:
        return result(models[normalized][0], "namespace_alias")
    if "/" not in mid:
        candidates = [v[0] for k, v in models.items() if k.rsplit("/", 1)[-1] == key]
        if len(candidates) == 1:
            return result(candidates[0], "unique_unqualified_id")
        if len(candidates) > 1:
            return result(method="ambiguous", candidates=sorted(candidates), reason="unqualified_id_is_not_unique")

    links = explicit_canonical_links(models, providers, mid)
    if links:
        targets = {target.casefold() for target, _ in links}
        sources = sorted(source for _, source in links)
        if len(targets) != 1:
            return result(method="ambiguous", sources=sources, candidates=sorted(targets), reason="explicit_canonical_conflict")
        target = next(iter(targets))
        conflict = identity_conflict(mid, target)
        if conflict:
            return result(sources=sources, candidates=[target], reason=conflict)
        if target not in models:
            return result(sources=sources, candidates=[target], reason="explicit_canonical_record_missing")
        return result(models[target][0], "provider_canonical", sources)

    # Candidate discovery stays within a manufacturer and an exact structural
    # model identity. Missing size can be resolved only when one candidate exists.
    query = model_identity(mid)
    candidates = []
    for _, (target, _) in models.items():
        profile = model_identity(target)
        if not query["namespace"] or query["namespace"] != profile["namespace"]:
            continue
        if any(query[k] != profile[k] for k in ("roles", "versions", "quantization")):
            continue
        if any(profile["sizes"].get(k) != value for k, value in query["sizes"].items()):
            continue
        if query["dates"] and query["dates"] != profile["dates"]:
            continue
        candidates.append((target, profile))
    # An undated canonical is a stronger match than dated historical variants.
    # If only dated variants exist, never silently choose one for an undated ID.
    exact_date = [(target, profile) for target, profile in candidates if profile["dates"] == query["dates"]]
    if exact_date:
        candidates = exact_date
    if len(candidates) > 1:
        return result(method="ambiguous", candidates=sorted(x[0] for x in candidates), reason="multiple_structural_candidates")
    if not candidates:
        return result(reason="no_compatible_structural_candidate")
    target, profile = candidates[0]
    if profile["dates"] != query["dates"]:
        return result(candidates=[target], reason="dated_variant_not_specified")
    reasons = []
    if profile["deployment"] != query["deployment"]:
        reasons.append("deployment_label_difference")
    if profile["sizes"] != query["sizes"]:
        reasons.append("omitted_parameter_size")
    return result(target, "structured_unique", reason=",".join(reasons) or "normalized_identity_tokens")


def match_models_dev(models, providers, mid):
    match = resolve_metadata(models, providers, mid)
    return match["canonical"], match["method"]


def validate_options(options, label):
    if not isinstance(options, list):
        fail(f"{label}: invalid reasoning_options")
    for option in options:
        if not isinstance(option, dict):
            fail(f"{label}: invalid reasoning option")
        kind = option.get("type")
        if kind == "toggle" and set(option) == {"type"}:
            continue
        if kind == "effort" and set(option) == {"type", "values"}:
            values = option["values"]
            allowed = {None, "none", "minimal", "low", "medium", "high", "xhigh", "max", "default"}
            if isinstance(values, list) and all(isinstance(x, str) or x is None for x in values):
                if len(values) == len(set(values)) and all(x in allowed for x in values):
                    continue
        if kind == "budget_tokens" and set(option) <= {"type", "min", "max"}:
            minimum, maximum = option.get("min"), option.get("max")
            if ((minimum is None or type(minimum) is int and minimum >= -1)
                    and (maximum is None or type(maximum) is int and maximum >= 0)
                    and (minimum is None or maximum is None or minimum <= maximum)):
                continue
        fail(f"{label}: unknown or invalid reasoning option: {option!r}")
    return options


def reasoning_reference(providers, mid, canonical):
    if not canonical:
        return None
    namespace, canonical_short = canonical.casefold().split("/", 1)
    provider_id = ORIGIN_PROVIDERS.get(namespace, namespace)
    offerings = providers.get(provider_id, {})
    original = origin_id(mid)
    short = original.split("/", 1)[1] if original.startswith(namespace + "/") else canonical_short
    # Prefer the original offering only if its explicit canonical mapping agrees.
    # A provider's latest alias may have advanced to a different model version.
    for key in dict.fromkeys((short, canonical_short)):
        found = offerings.get(key)
        if found:
            target = found[1].get("canonical_model_id")
            if target is not None and not isinstance(target, str):
                fail(f"{provider_id}/{key}: invalid canonical_model_id")
            if target is None or target.casefold() == canonical.casefold():
                if "reasoning_options" not in found[1]:
                    return None
                options = validate_options(found[1]["reasoning_options"], provider_id + "/" + key)
                return {"provider": provider_id, "model_id": found[0], "options": options}
    # Multiple first-party aliases may explicitly identify the same canonical.
    # Accept their reference options only when ALL agree; do not union capabilities.
    aliases = sorted((key, model) for key, model in offerings.values()
                     if model.get("canonical_model_id", "").casefold() == canonical.casefold())
    if not aliases or any("reasoning_options" not in model for _, model in aliases):
        return None
    options = [validate_options(model["reasoning_options"], provider_id + "/" + key)
               for key, model in aliases]
    if any(value != options[0] for value in options[1:]):
        return None
    return {"provider": provider_id, "model_id": aliases[0][0], "options": options[0]}


def build_models(roster, metadata):
    models, providers = metadata
    result = []
    for row in roster:
        mid = row["id"]
        match = resolve_metadata(models, providers, mid)
        target, method = match["canonical"], match["method"]
        meta = models[target.casefold()][1] if target else {}
        limits = meta.get("limit", {})
        if not isinstance(limits, dict):
            fail(f"{target}: invalid limits")
        context, output = limits.get("context", 0), limits.get("output", 0)
        if any(type(value) is not int or not 0 <= value <= MAX_TOKENS for value in (context, output)):
            fail(f"{target}: token limits must be nonnegative int64 values")
        if context and output > context:
            fail(f"{target}: output exceeds context")
        context_source = "modelsdev" if context else "fallback"
        output_source = "modelsdev" if output else "fallback"
        if not output:
            output = min(FALLBACK_PROFILE["output"], context) if context else FALLBACK_PROFILE["output"]
        if not context:
            context = FALLBACK_PROFILE["context"]
        reasoning = meta.get("reasoning")
        if reasoning is not None and type(reasoning) is not bool:
            fail(f"{target}: invalid reasoning boolean")
        tool_call = meta.get("tool_call")
        if tool_call is not None and type(tool_call) is not bool:
            fail(f"{target}: invalid tool_call boolean")
        modalities = meta.get("modalities", {})
        if not isinstance(modalities, dict):
            fail(f"{target}: invalid modalities")
        inputs = modalities.get("input")
        if inputs is not None and (not isinstance(inputs, list) or any(not isinstance(x, str) for x in inputs)):
            fail(f"{target}: invalid input modalities")
        model_name, description = meta.get("name", row["display"]), meta.get("description", "")
        display = row["display"]  # Preserve deployment labels such as Fast/Free.
        if not isinstance(model_name, str) or not model_name.strip() or not isinstance(description, str):
            fail(f"{target or mid}: invalid name/description")
        ref = "models/" + target if target else ""
        result.append({
            "id": mid, "display": display, "description": description,
            "min_plan": "Go", "min_plan_source": SOURCE,
            "context": context, "context_source": context_source,
            "output": output, "output_source": output_source,
            "output_ref": ref if output_source == "modelsdev" else "",
            "vision": "image" in inputs if inputs is not None else None,
            "vision_source": "modelsdev" if inputs is not None else "unresolved",
            "reasoning": reasoning,
            "reasoning_source": "modelsdev" if reasoning is not None else "unresolved",
            "tool_call": tool_call, "tool_call_source": "modelsdev" if tool_call is not None else "unresolved",
            "reasoning_options_reference": reasoning_reference(providers, mid, target),
            "reasoning_efforts": [], "reasoning_efforts_source": "unresolved",
            "metadata_ref": ref, "metadata_match": method,
            "metadata_match_sources": match["sources"],
            "metadata_match_reason": match["reason"],
            "metadata_candidates": match["candidates"],
        })
    return sorted(result, key=lambda m: m["id"].casefold())


def validate_drift(models, previous, allow=False):
    old = {m["id"] for m in previous}
    new = {m["id"] for m in models}
    if old:
        added, removed = new - old, old - new
        print(f"roster diff: +{len(added)} -{len(removed)}", file=sys.stderr)
        if added or removed:
            print(f"added={sorted(added)} removed={sorted(removed)}", file=sys.stderr)
        if max(len(added), len(removed)) > max(10, len(old) * .25) and not allow:
            fail("large roster drift; review and use --allow-large-drift")


def fetch_source(url):
    """Bounded public GET only. Failure never replaces generated outputs."""
    if url not in (GO_URL, MODELS_DEV_URL):
        fail("only the two public catalog source URLs may be fetched")
    expected_type = "application/json" if url == MODELS_DEV_URL else "text/html"
    request = urllib.request.Request(url, headers={"User-Agent": "cpa-cmdcode-go-catalog/1", "Accept": expected_type})
    for attempt in range(3):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                if response.status != 200 or not response.geturl().startswith("https://"):
                    fail(f"unexpected response for {url}")
                content_type = response.headers.get("Content-Type", "").split(";", 1)[0].strip().lower()
                if content_type != expected_type:
                    fail(f"unexpected Content-Type for {url}: {content_type!r}")
                body = response.read(MAX_SOURCE_BYTES + 1)
                if not body or len(body) > MAX_SOURCE_BYTES:
                    fail(f"empty or oversized source: {url}")
                return body
        except urllib.error.HTTPError as exc:
            if exc.code not in (429, 500, 502, 503, 504) or attempt == 2:
                raise
        except (urllib.error.URLError, TimeoutError):
            if attempt == 2:
                raise
        time.sleep(attempt + 1)
    fail(f"could not fetch {url}")


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def render_go(doc, json_bytes):
    lines = ["// Code generated by scripts/extract-models.py. DO NOT EDIT.",
             "// Model snapshot SHA-256: " + sha256(json_bytes), "package main", "",
             "var modelTable = []modelDef{"]
    for m in doc["models"]:
        fields = ["id: " + json.dumps(m["id"], ensure_ascii=False),
                  "display: " + json.dumps(m["display"], ensure_ascii=False),
                  "description: " + json.dumps(m["description"], ensure_ascii=False),
                  "context: " + str(m["context"]), "output: " + str(m["output"]),
                  "contextSource: " + json.dumps(m["context_source"]),
                  "outputSource: " + json.dumps(m["output_source"]),
                  "efforts: []string{" + ", ".join(json.dumps(x) for x in m["reasoning_efforts"]) + "}"]
        if m["vision"] is not None:
            fields.append("vision: modelBool(" + str(m["vision"]).lower() + ")")
        lines.append("\t{" + ", ".join(fields) + "},")
    lines.extend(["}", ""])
    return "\n".join(lines).encode()


def stage_file(path, data):
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix="." + path.name + ".", delete=False) as stream:
        name = Path(stream.name)
        try:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
            os.chmod(name, 0o644)
        except BaseException:
            name.unlink(missing_ok=True)
            raise
    return name


def replace_outputs(outputs):
    """Atomic per file; rollback ordinary errors. CI detects crash-time mixing."""
    staged, backups, written = {}, {}, []
    try:
        for path, data in outputs.items():
            backups[path] = path.read_bytes() if path.exists() else None
            staged[path] = stage_file(path, data)
        for path, temporary in staged.items():
            os.replace(temporary, path)
            written.append(path)
    except BaseException:
        for path in reversed(written):
            if backups[path] is None:
                path.unlink(missing_ok=True)
            else:
                recovery = stage_file(path, backups[path])
                os.replace(recovery, path)
        raise
    finally:
        for temporary in staged.values():
            temporary.unlink(missing_ok=True)


def validate_document(doc):
    if doc.get("schema_version") != 1 or doc.get("source") != SOURCE:
        fail("unexpected generated catalog source/schema")
    models = doc.get("models")
    if not isinstance(models, list) or not 10 <= len(models) <= 300:
        fail("invalid generated model count")
    valid_ids([m["id"] for m in models], "generated models")
    shorts = [m["id"].rsplit("/", 1)[-1].casefold() for m in models]
    if any(not short for short in shorts) or len(set(shorts)) != len(shorts):
        fail("generated models have empty or colliding public short IDs")
    if doc.get("counts", {}).get("entitled") != len(models):
        fail("generated entitled count disagrees with models")
    for model in models:
        if (not isinstance(model.get("display"), str) or not model["display"].strip()
                or not isinstance(model.get("description"), str)):
            fail("generated display/description must be strings")
        for key in ("context", "output"):
            value = model.get(key)
            if type(value) is not int or not 0 <= value <= MAX_TOKENS:
                fail(f"generated {key} must be a nonnegative int64")
        if model["context_source"] != "fallback" and model["context"] and model["output"] > model["context"]:
            fail("generated output exceeds context")
        for key in ("vision", "reasoning", "tool_call"):
            if model.get(key) is not None and type(model[key]) is not bool:
                fail(f"invalid generated {key}")
        if model.get("reasoning_efforts") != [] or model.get("reasoning_efforts_source") != "unresolved":
            fail("these sources do not verify Command Code reasoning efforts")
        if model.get("gateway_output_limit"):
            fail("these sources do not verify a Command Code gateway hard limit")


def check_generated(out, gen):
    raw = out.read_bytes()
    doc = parse_json(raw)
    validate_document(doc)
    if gen.read_bytes() != render_go(doc, raw):
        fail("model snapshot and generated Go differ; regenerate with --from-json")


def semantic_catalog(doc):
    """Ignore observations outside the published Go roster, not model metadata.

    Keep provenance from the last meaningful publication. Otherwise unrelated
    models.dev records, page HTML, and timestamps create pointless commits.
    """
    return {k: ({ck: cv for ck, cv in v.items() if ck != "catalog"}
                if k == "counts" else v)
            for k, v in doc.items()
            if k not in {"fetched_at", "source_sha256", "models_dev_sha256"}}


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--go-page", help="local raw HTML snapshot (otherwise fetch public URL)")
    ap.add_argument("--models-dev", help="local catalog.json snapshot (otherwise fetch public URL)")
    ap.add_argument("--prev", default="")
    ap.add_argument("--out", default="data/models.snapshot.json")
    ap.add_argument("--gen", default="go/models_generated.go")
    ap.add_argument("--allow-large-drift", action="store_true")
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true", help="offline snapshot/Go consistency check")
    mode.add_argument("--from-json", help="reproduce compiled fallback from an exact JSON snapshot, offline")
    mode.add_argument("--json-only", action="store_true", help="publish JSON without modifying the compiled fallback")
    a = ap.parse_args()
    out, gen = Path(a.out), Path(a.gen)
    if out.resolve() == gen.resolve():
        fail("JSON and Go output paths must differ")
    if a.from_json:
        if a.go_page or a.models_dev or a.prev:
            fail("--from-json cannot be combined with source snapshots or --prev")
        raw = Path(a.from_json).read_bytes()
        doc = parse_json(raw)
        validate_document(doc)
        replace_outputs({gen: render_go(doc, raw)})
        return
    if a.check:
        check_generated(out, gen)
        return
    if bool(a.go_page) != bool(a.models_dev):
        fail("provide both local source snapshots, or neither to fetch")
    page = Path(a.go_page).read_bytes() if a.go_page else fetch_source(GO_URL)
    metadata = Path(a.models_dev).read_bytes() if a.models_dev else fetch_source(MODELS_DEV_URL)
    if max(len(page), len(metadata)) > MAX_SOURCE_BYTES:
        fail("source snapshot too large")
    roster, total = parse_go_page(page.decode("utf-8"))
    models = build_models(roster, load_models_dev(metadata.decode("utf-8")))
    previous_raw = Path(a.prev).read_bytes() if a.prev and Path(a.prev).exists() else None
    previous = parse_json(previous_raw) if previous_raw is not None else {}
    if previous:
        validate_document(previous)
    validate_drift(models, previous.get("models", []), a.allow_large_drift)
    doc = {"schema_version": 1, "source": SOURCE, "source_url": GO_URL,
           "source_sha256": sha256(page), "models_dev_url": MODELS_DEV_URL,
           "models_dev_sha256": sha256(metadata), "fallback_profile": FALLBACK_PROFILE,
           "counts": {"catalog": total, "entitled": len(models)}, "models": models}
    doc["fetched_at"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    validate_document(doc)
    if previous and semantic_catalog(doc) == semantic_catalog(previous):
        # Retain exact bytes and their original provenance, not just fetched_at.
        doc, json_bytes = previous, previous_raw
    else:
        json_bytes = (json.dumps(doc, ensure_ascii=False, indent=2) + "\n").encode()
    outputs = {out: json_bytes}
    if not a.json_only:
        outputs[gen] = render_go(doc, json_bytes)
    replace_outputs(outputs)
    print(f"Go page: catalog={total} Go={len(models)} metadata={sum(bool(m['metadata_ref']) for m in models)}")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, TypeError) as exc:
        print(f"extract-models: error: {exc}", file=sys.stderr)
        sys.exit(2)
