import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import urllib.error

spec = importlib.util.spec_from_file_location("extract", Path(__file__).with_name("extract-models.py"))
extract = importlib.util.module_from_spec(spec)
spec.loader.exec_module(extract)
FIXTURES = Path(__file__).with_name("fixtures")
SCRIPT = Path(__file__).with_name("extract-models.py")


def page(props, cuts=()):
    text = '7:["$","$L4",null,' + json.dumps(props) + ']\n'
    positions = (0, *cuts, len(text))
    return "".join('<script>self.__next_f.push(' + json.dumps([1, text[a:b]]) + ')</script>'
                   for a, b in zip(positions, positions[1:]))


def metadata(records, providers=None):
    return extract.load_models_dev(json.dumps({
        "models": {k: {"id": k, **v} for k, v in records.items()},
        "providers": providers or {},
    }))


def provider(name, records):
    return {name: {"id": name, "models": {k: {"id": k, **v} for k, v in records.items()}}}


class ExtractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.html = (FIXTURES / "commandcode-go-page.html").read_text()
        cls.metadata_text = (FIXTURES / "modelsdev-catalog.sample.json").read_text()

    def setUp(self):
        self.roster, self.total = extract.parse_go_page(self.html)
        self.props = {"planScope": {"label": "Go plan", "modelIds": [r["id"] for r in self.roster]},
                      "models": [{**r, "name": r["display"], "minPlanName": "Go"} for r in self.roster]}

    def test_real_page_membership(self):
        self.assertEqual(len(self.roster), 56)
        self.assertEqual(self.total, 94)
        self.assertIn("deepseek/deepseek-v4.1-flash-fast", {r["id"] for r in self.roster})
        self.assertNotIn("gpt-6-astra", {r["id"] for r in self.roster})
        self.assertTrue(all(set(row) == {"id", "display"} for row in self.roster))

    def test_chunk_boundaries_and_record_numbers_are_not_fixed(self):
        html = page(self.props, (1, 2, 13, 78, 200, 333))
        self.assertEqual(extract.parse_go_page(html)[0], self.roster)
        self.assertEqual(extract.parse_go_page(page(self.props).replace('7:', 'a31:'))[0], self.roster)

    def test_page_schema_changes_fail_closed(self):
        variants = ["", '<html>Access denied</html>', page({}), page({"planScope": []})]
        for alter in (lambda p: p["planScope"].update(label="All plans"),
                      lambda p: p["planScope"].update(modelIds=[]),
                      lambda p: p["planScope"]["modelIds"].append(p["planScope"]["modelIds"][0].upper()),
                      lambda p: p["planScope"]["modelIds"].append("foreign/not-in-page"),
                      lambda p: p["models"][0].update(minPlanName="Pro"),
                      lambda p: p["models"][0].update(name="")):
            props = copy.deepcopy(self.props)
            alter(props)
            variants.append(page(props))
        variants.extend([page(self.props) * 2, page(self.props).replace(')</script>', ');evil()</script>')])
        for html in variants:
            with self.subTest(html=html[:100]), self.assertRaises(ValueError):
                extract.parse_go_page(html)

    def test_json_duplicate_keys_and_nonfinite_numbers(self):
        for text in ('{"models":{},"models":{}}', '{"x":NaN}', '{"x":Infinity}'):
            with self.assertRaises(ValueError):
                extract.parse_json(text)

    def test_modelsdev_shape_validation(self):
        for text in ('{}', '{"models":[],"providers":{}}', '{"models":{"a":{"id":"b"}},"providers":{}}'):
            with self.assertRaises(ValueError):
                extract.load_models_dev(text)
        # Other providers legitimately use @ in model IDs; do not reject them.
        metadata({"lab/a": {}}, provider("cloudflare", {"@cf/lab/a": {}}))

    def test_matching_exact_and_explicit_namespace_aliases(self):
        meta = metadata({"alibaba/Qwen3.8-Max": {}, "minimax/MiniMax-M3": {}, "zhipuai/glm-5.2": {}})
        for mid, expected in (("Qwen/Qwen3.8-Max", "alibaba/Qwen3.8-Max"),
                              ("MiniMaxAI/MiniMax-M3", "minimax/MiniMax-M3"),
                              ("zai-org/GLM-5.2", "zhipuai/glm-5.2")):
            self.assertEqual(extract.match_models_dev(*meta, mid), (expected, "namespace_alias"))
        self.assertIsNone(extract.match_models_dev(*meta, "foreign/glm-5.2")[0])

    def test_exact_variant_beats_provider_canonical_and_suffix(self):
        meta = metadata({"lab/a-fast": {}, "lab/a": {}, "lab/a-new": {}},
                        provider("lab", {"a-fast": {"canonical_model_id": "lab/a-new"}}))
        self.assertEqual(extract.match_models_dev(*meta, "LAB/A-FAST"), ("lab/a-fast", "exact"))

    def test_provider_canonical_mapping_precedes_suffix(self):
        meta = metadata({"lab/a": {}, "lab/a-120b": {}},
                        provider("lab", {"a-fast": {"canonical_model_id": "lab/a-120b"}}))
        self.assertEqual(extract.match_models_dev(*meta, "lab/a-fast"), ("lab/a-120b", "provider_canonical"))

    def test_controlled_fast_fallback_and_no_arbitrary_suffix_removal(self):
        meta = metadata({"deepseek/deepseek-v4.1-flash": {}})
        self.assertEqual(extract.match_models_dev(*meta, "deepseek/deepseek-v4.1-flash-fast"),
                         ("deepseek/deepseek-v4.1-flash", "structured_unique"))
        for mid in ("deepseek-v4.1-flash-thinking", "deepseek-v4.1-flash-fp8",
                    "deepseek-v4.1-flash-128k", "deepseek-v41-flash"):
            self.assertIsNone(extract.match_models_dev(*meta, "deepseek/" + mid)[0])

    def test_unqualified_id_requires_unique_candidate(self):
        one = metadata({"one/a": {}})
        self.assertEqual(extract.match_models_dev(*one, "a"), ("one/a", "unique_unqualified_id"))
        two = metadata({"one/a": {}, "two/a": {}})
        self.assertEqual(extract.match_models_dev(*two, "a"), (None, "ambiguous"))
        self.assertEqual(extract.match_models_dev(*two, "a-fast"), (None, "unresolved"))

    def test_unique_parameter_extension_and_ambiguous_sizes(self):
        one = metadata({"nvidia/nemotron-3.5-super-vl-120b-a12b": {}})
        match = extract.resolve_metadata(*one, "nvidia/nemotron-3.5-super-vl")
        self.assertEqual(match["canonical"], "nvidia/nemotron-3.5-super-vl-120b-a12b")
        self.assertEqual(match["reason"], "omitted_parameter_size")
        two = metadata({"nvidia/nemotron-3.5-super-vl-120b-a12b": {}, "nvidia/nemotron-3.5-super-vl-30b-a3b": {}})
        match = extract.resolve_metadata(*two, "nvidia/nemotron-3.5-super-vl")
        self.assertEqual(match["method"], "ambiguous")
        self.assertEqual(len(match["candidates"]), 2)

    def test_structural_conflicts_and_commercial_labels(self):
        meta = metadata({"lab/a-v4.1-flash-120b-a12b": {}, "tencent/hy3": {}, "tencent/hy3-preview": {}})
        for mid in ("lab/a-v4-flash", "lab/a-v4.1-pro", "lab/a-v4.1-flash-vl",
                    "lab/a-v4.1-flash-27b", "lab/a-v4.1-flash-fp8", "lab/a-v4.1-flash-thinking"):
            self.assertIsNone(extract.match_models_dev(*meta, mid)[0])
        self.assertEqual(extract.match_models_dev(*meta, "tencent/hy3-paid"), ("tencent/hy3", "structured_unique"))
        self.assertEqual(extract.model_identity("lab/a-2.4t-a95b")["sizes"],
                         extract.model_identity("lab/a-2400b-a95b")["sizes"])

    def test_dated_candidates_are_not_arbitrarily_selected(self):
        meta = metadata({"lab/a-0902": {}, "lab/a-0825": {}})
        result = extract.resolve_metadata(*meta, "lab/a-fast")
        self.assertEqual(result["method"], "ambiguous")
        self.assertEqual(result["candidates"], ["lab/a-0825", "lab/a-0902"])
        one = metadata({"lab/a-0902": {}})
        self.assertEqual(extract.resolve_metadata(*one, "lab/a-fast")["reason"], "dated_variant_not_specified")
        with_base = metadata({"lab/a": {}, "lab/a-0902": {}, "lab/a-0825": {}})
        self.assertEqual(extract.match_models_dev(*with_base, "lab/a-fast"), ("lab/a", "structured_unique"))

    def test_explicit_identity_conflicts_stop_before_fuzzy(self):
        for query, wrong in (("lab/a-0902", "lab/a-0825"), ("lab/a-thinking", "lab/a-instruct"),
                             ("lab/a-27b", "lab/a-70b"), ("lab/a", "other/a"),
                             ("lab/a-v4", "lab/a-v4.1"), ("lab/a-fp8", "lab/a-int4"),
                             ("lab/a-q4_k_m", "lab/a-q4_k_s"), ("lab/a", "lab/b"),
                             ("lab/a-v4-fp8", "lab/a-v4"), ("lab/a-v4", "lab/a-v4-fp8"),
                             ("lab/a-v4-27b", "lab/a-v4"), ("lab/a-v4-vl", "lab/a-v4"),
                             ("lab/a-v4-0902", "lab/a-v4"), ("lab/a-v4", "lab/a"),
                             ("lab/a-v4", "lab/a-v4-0902"), ("lab/a-thinking", "lab/a"),
                             ("lab/a-v4-pro", "lab/a-v4"), ("lab/a-v4-flash", "lab/a-v4")):
            models = {wrong: {}}
            # A plausible same-family candidate must not erase explicit conflicts.
            models[query + "-120b"] = {}
            meta = metadata(models, provider("host", {query: {"canonical_model_id": wrong}}))
            with self.subTest(query=query, wrong=wrong):
                result = extract.resolve_metadata(*meta, query)
                self.assertIsNone(result["canonical"])
                self.assertTrue(result["reason"].endswith("_conflict"))
        meta = metadata({"lab/a-120b": {}, "lab/a-30b": {}}, {
            **provider("host1", {"lab/a": {"canonical_model_id": "lab/a-120b"}}),
            **provider("host2", {"lab/a": {"canonical_model_id": "lab/a-30b"}})})
        result = extract.resolve_metadata(*meta, "lab/a")
        self.assertEqual(result["reason"], "explicit_canonical_conflict")
        self.assertEqual(len(result["sources"]), 2)

    def test_full_id_provider_evidence_is_not_a_capability_source(self):
        meta = metadata({"lab/a-120b": {"reasoning": True}},
                        provider("reseller", {"lab/a": {"canonical_model_id": "lab/a-120b", "reasoning_options": [{"type": "toggle"}]}}))
        row = extract.build_models([{"id": "lab/a", "display": "A"}], meta)[0]
        self.assertEqual(row["metadata_ref"], "models/lab/a-120b")
        self.assertEqual(row["metadata_match_sources"], ["providers/reseller/models/lab/a"])
        self.assertIsNone(row["reasoning_options_reference"])

    def test_duplicate_provider_casefold_and_malformed_canonical(self):
        for providers in ({**provider("test", {}), **provider("TEST", {})},
                          provider("test", {"a": {"canonical_model_id": None}})):
            with self.assertRaises(ValueError):
                metadata({"lab/a": {}}, providers)

    def test_reference_options_do_not_become_gateway_efforts(self):
        rows = extract.build_models(self.roster, extract.load_models_dev(self.metadata_text))
        by_id = {m["id"]: m for m in rows}
        mimo = by_id["xiaomi/mimo-v2.6-pro"]
        kimi = by_id["moonshotai/Kimi-K3"]
        self.assertTrue(mimo["reasoning"])
        self.assertEqual(mimo["context"], 1048576)
        self.assertEqual(mimo["output"], 131072)
        self.assertEqual(mimo["reasoning_options_reference"]["options"], [{"type": "toggle"}])
        self.assertEqual(kimi["reasoning_options_reference"]["options"], [{"type": "effort", "values": ["low", "high", "max"]}])
        self.assertTrue(all(m["reasoning_efforts"] == [] and m["reasoning_efforts_source"] == "unresolved" for m in rows))
        self.assertEqual(len(rows), 56)

    def test_provider_mapping_to_another_version_is_not_a_reference(self):
        meta = metadata({"lab/a": {"reasoning": True}},
                        provider("lab", {"a": {"canonical_model_id": "lab/a-new", "reasoning_options": [{"type": "toggle"}]}}))
        row = extract.build_models([{"id": "lab/a", "display": "A"}], meta)[0]
        self.assertIsNone(row["reasoning_options_reference"])

    def test_unknown_is_not_false_and_empty_options_are_not_unknown(self):
        meta = metadata({"lab/a": {"reasoning": False}, "lab/b": {"reasoning": True}},
                        provider("lab", {"b": {"reasoning_options": []}}))
        rows = extract.build_models([{"id": x, "display": x} for x in ("lab/a", "lab/b", "missing/model")], meta)
        by_id = {r["id"]: r for r in rows}
        self.assertFalse(by_id["lab/a"]["reasoning"])
        self.assertEqual(by_id["lab/b"]["reasoning_options_reference"]["options"], [])
        unknown = by_id["missing/model"]
        self.assertIsNone(unknown["reasoning"])
        self.assertIsNone(unknown["vision"])
        self.assertEqual((unknown["context"], unknown["output"]), (262144, 65536))
        self.assertEqual(unknown["context_source"], "fallback")
        self.assertIsNone(unknown["tool_call"])
        self.assertEqual(unknown["output_source"], "fallback")

    def test_partial_fallback_never_overwrites_known_limits(self):
        meta = metadata({"lab/a": {"limit": {"context": 4096}},
                         "lab/b": {"limit": {"output": 500000}}})
        rows = extract.build_models([{"id": x, "display": x} for x in ("lab/a", "lab/b")], meta)
        self.assertEqual((rows[0]["context"], rows[0]["output"]), (4096, 4096))
        self.assertEqual((rows[0]["context_source"], rows[0]["output_source"]), ("modelsdev", "fallback"))
        self.assertEqual((rows[1]["context"], rows[1]["output"]), (262144, 500000))
        self.assertEqual((rows[1]["context_source"], rows[1]["output_source"]), ("fallback", "modelsdev"))

    def test_original_ids_and_distinct_deployments_are_preserved(self):
        meta = metadata({"lab/a": {}})
        rows = extract.build_models([{"id": x, "display": x} for x in ("lab/a", "lab/A-fast", "foreign/a")], meta)
        self.assertEqual({x["id"] for x in rows}, {"lab/a", "lab/A-fast", "foreign/a"})
        self.assertEqual(sum(x["metadata_ref"] == "models/lab/a" for x in rows), 2)

    def test_invalid_model_limits_are_not_rounded_or_defaulted(self):
        for limits in ({"context": True}, {"context": -1}, {"context": 1.5}, {"output": 1 << 63},
                       {"context": 100, "output": 101}, {"output": "100"}):
            with self.subTest(limits=limits), self.assertRaises(ValueError):
                extract.build_models([{"id": "lab/a", "display": "A"}], metadata({"lab/a": {"limit": limits}}))
        row = extract.build_models([{"id": "lab/a", "display": "A"}], metadata({"lab/a": {"limit": {"output": 100}}}))[0]
        self.assertEqual((row["context"], row["output"]), (262144, 100))

    def test_invalid_names_and_descriptions_rejected(self):
        for patch in ({"name": None}, {"name": ""}, {"description": {}}, {"description": None}):
            with self.assertRaises(ValueError):
                extract.build_models([{"id": "lab/a", "display": "A"}], metadata({"lab/a": patch}))

    def test_aliases_with_disagreeing_reference_options_stay_unknown(self):
        meta = metadata({"lab/a": {}}, provider("lab", {
            "alias1": {"canonical_model_id": "lab/a", "reasoning_options": [{"type": "toggle"}]},
            "alias2": {"canonical_model_id": "lab/a", "reasoning_options": []}}))
        self.assertIsNone(extract.reasoning_reference(meta[1], "lab/a", "lab/a"))

    def test_reasoning_options_validation(self):
        valid = [{"type": "toggle"}, {"type": "effort", "values": [None, "none", "low", "max"]},
                 {"type": "budget_tokens", "min": -1, "max": 1000}]
        self.assertEqual(extract.validate_options(valid, "test"), valid)
        for options in ([{"type": "new"}], [{"type": "effort", "values": ["invented"]}],
                        [{"type": "toggle", "default": True}], [{"type": "budget_tokens", "min": 2, "max": 1}]):
            with self.assertRaises(ValueError):
                extract.validate_options(options, "test")

    def test_large_drift_requires_review_but_small_additions_work(self):
        old = [{"id": str(i)} for i in range(40)]
        extract.validate_drift(old + [{"id": "new"}], old)
        new = [{"id": str(i)} for i in range(80)]
        with self.assertRaises(ValueError):
            extract.validate_drift(new, old)
        extract.validate_drift(new, old, True)

    def test_write_error_rolls_back_both_files(self):
        with tempfile.TemporaryDirectory() as root:
            a, b = Path(root) / "a", Path(root) / "b"
            a.write_bytes(b"old-a")
            b.write_bytes(b"old-b")
            replace = extract.os.replace
            calls = []
            def broken_replace(src, dst):
                calls.append(dst)
                if len(calls) == 2:
                    raise OSError("simulated second-file failure")
                return replace(src, dst)
            with mock.patch.object(extract.os, "replace", side_effect=broken_replace), self.assertRaises(OSError):
                extract.replace_outputs({a: b"new-a", b: b"new-b"})
            self.assertEqual((a.read_bytes(), b.read_bytes()), (b"old-a", b"old-b"))
            self.assertEqual(sorted(p.name for p in Path(root).iterdir()), ["a", "b"])

    def test_fetch_retry_and_permanent_failure(self):
        response = mock.MagicMock()
        response.__enter__.return_value = response
        response.status = 200
        response.headers = {"Content-Type": "text/html; charset=utf-8"}
        response.geturl.return_value = extract.GO_URL
        response.read.return_value = b"actual response"
        with mock.patch.object(extract.urllib.request, "urlopen", side_effect=[urllib.error.URLError("temporary"), response]) as get, mock.patch.object(extract.time, "sleep"):
            self.assertEqual(extract.fetch_source(extract.GO_URL), b"actual response")
            self.assertEqual(get.call_count, 2)
        with mock.patch.object(extract.urllib.request, "urlopen", side_effect=urllib.error.HTTPError(extract.GO_URL, 403, "Forbidden", {}, None)) as get, self.assertRaises(urllib.error.HTTPError):
            extract.fetch_source(extract.GO_URL)
        self.assertEqual(get.call_count, 1)

    def test_network_failure_and_wrong_content_type_do_not_replace_outputs(self):
        with tempfile.TemporaryDirectory() as root:
            out, gen = Path(root) / "models.json", Path(root) / "models.go"
            out.write_bytes(b"last good JSON")
            gen.write_bytes(b"last good Go")
            argv = [str(SCRIPT), "--out", str(out), "--gen", str(gen)]
            with mock.patch.object(sys, "argv", argv), mock.patch.object(extract, "fetch_source", side_effect=[self.html.encode(), urllib.error.URLError("outage")]), self.assertRaises(urllib.error.URLError):
                extract.main()
            self.assertEqual(out.read_bytes(), b"last good JSON")
            self.assertEqual(gen.read_bytes(), b"last good Go")
        response = mock.MagicMock()
        response.__enter__.return_value = response
        response.status = 200
        response.geturl.return_value = extract.MODELS_DEV_URL
        response.headers = {"Content-Type": "text/html"}
        with mock.patch.object(extract.urllib.request, "urlopen", return_value=response), self.assertRaises(ValueError):
            extract.fetch_source(extract.MODELS_DEV_URL)

    def test_empty_and_oversized_sources_rejected(self):
        for body in (b"", b"12345"):
            response = mock.MagicMock()
            response.__enter__.return_value = response
            response.status = 200
            response.headers = {"Content-Type": "text/html"}
            response.geturl.return_value = extract.GO_URL
            response.read.return_value = body
            with mock.patch.object(extract, "MAX_SOURCE_BYTES", 4), mock.patch.object(extract.urllib.request, "urlopen", return_value=response), self.assertRaises(ValueError):
                extract.fetch_source(extract.GO_URL)

    def test_end_to_end_consistency_determinism_and_failed_input(self):
        with tempfile.TemporaryDirectory() as root:
            out, gen = Path(root) / "models.json", Path(root) / "models.go"
            args = [sys.executable, str(SCRIPT), "--go-page", str(FIXTURES / "commandcode-go-page.html"),
                    "--models-dev", str(FIXTURES / "modelsdev-catalog.sample.json"),
                    "--out", str(out), "--gen", str(gen), "--prev", str(out)]
            subprocess.run(args, check=True, capture_output=True)
            first = out.read_bytes(), gen.read_bytes()
            subprocess.run(args, check=True, capture_output=True)
            self.assertEqual(first, (out.read_bytes(), gen.read_bytes()))
            extract.check_generated(out, gen)
            bad = Path(root) / "broken.html"
            bad.write_text("upstream outage")
            args[args.index("--go-page") + 1] = str(bad)
            self.assertNotEqual(subprocess.run(args, capture_output=True).returncode, 0)
            self.assertEqual(first, (out.read_bytes(), gen.read_bytes()))
            args[args.index("--go-page") + 1] = str(FIXTURES / "commandcode-go-page.html")
            broken_meta = Path(root) / "metadata.json"
            args[args.index("--models-dev") + 1] = str(broken_meta)
            for patch in ({"name": None}, {"description": {}}, {"limit": {"output": 1 << 63}}):
                source = json.loads(self.metadata_text)
                source["models"]["xiaomi/mimo-v2.6-pro"].update(patch)
                broken_meta.write_text(json.dumps(source))
                self.assertNotEqual(subprocess.run(args, capture_output=True).returncode, 0)
                self.assertEqual(first, (out.read_bytes(), gen.read_bytes()))
            out.write_bytes(out.read_bytes().replace(b'"context": 1048576', b'"context": 1048575', 1))
            with self.assertRaises(ValueError):
                extract.check_generated(out, gen)

    def test_semantic_identity_ignores_only_observation_noise(self):
        doc = {"schema_version": 1, "source": "source", "models": [{"id": "test/a", "context": 100}],
               "counts": {"catalog": 90, "entitled": 1}, "source_sha256": "old",
               "models_dev_sha256": "old", "fetched_at": "old"}
        noise = copy.deepcopy(doc)
        noise.update(source_sha256="new", models_dev_sha256="new", fetched_at="new")
        noise["counts"]["catalog"] = 100
        self.assertEqual(extract.semantic_catalog(doc), extract.semantic_catalog(noise))
        for patch in ({"context": 200}, {"description": "changed"}, {"metadata_ref": "different"}):
            changed = copy.deepcopy(noise)
            changed["models"][0].update(patch)
            self.assertNotEqual(extract.semantic_catalog(doc), extract.semantic_catalog(changed))
        noise["counts"]["entitled"] = 2
        self.assertNotEqual(extract.semantic_catalog(doc), extract.semantic_catalog(noise))

    def test_json_only_preserves_exact_bytes_for_irrelevant_source_changes(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            out, gen = root / "models.json", root / "models.go"
            html, metadata = root / "page.html", root / "catalog.json"
            html.write_text(self.html)
            metadata.write_text(self.metadata_text)
            args = [str(SCRIPT), "--json-only", "--go-page", str(html),
                    "--models-dev", str(metadata), "--prev", str(out),
                    "--out", str(out), "--gen", str(gen)]
            gen.write_text("do not touch the pinned fallback")
            with mock.patch.object(sys, "argv", args):
                extract.main()
                first = out.read_bytes()
                # Both complete-source hashes change, but neither change affects
                # the Go roster or its selected metadata.
                html.write_text(self.html + "\n<!-- unrelated site markup -->\n")
                source = json.loads(self.metadata_text)
                source["unrelated_catalog_metadata"] = "changed"
                metadata.write_text(json.dumps(source))
                extract.main()
                self.assertEqual(first, out.read_bytes())
                source["models"]["xiaomi/mimo-v2.6-pro"]["description"] = "Updated model reference."
                metadata.write_text(json.dumps(source))
                extract.main()
                self.assertNotEqual(first, out.read_bytes())
            self.assertEqual(gen.read_text(), "do not touch the pinned fallback")

    def test_exact_snapshot_reproduces_fallback_without_network(self):
        snapshot = SCRIPT.parent.parent / "data/models.snapshot.json"
        with tempfile.TemporaryDirectory() as root:
            gen = Path(root) / "models.go"
            args = [str(SCRIPT), "--from-json", str(snapshot), "--gen", str(gen)]
            with mock.patch.object(sys, "argv", args), mock.patch.object(extract, "fetch_source") as fetch:
                extract.main()
                first = gen.read_bytes()
                extract.main()
                fetch.assert_not_called()
            self.assertEqual(first, gen.read_bytes())
            self.assertEqual(first, (SCRIPT.parent.parent / "go/models_generated.go").read_bytes())
            extract.check_generated(snapshot, gen)


if __name__ == "__main__":
    unittest.main()
