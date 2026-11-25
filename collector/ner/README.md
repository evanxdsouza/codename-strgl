# Optional entity model for redaction

The collector's redaction rules catch secrets, contact details and names
that the text announces ("my name is", "Mr.", "Author:"). They miss names
with no such cue, such as "Built by Omar Haddad and Grace Lee". This folder
holds an optional helper that runs a small named-entity model on your
device to catch those too. It is off unless you turn it on, and it never
runs during normal tracking: only when you build, preview or draft a
devlog for a submission.

**Status: `dslim/distilbert-NER` accepted as an opt-in layer, below
target** (decided 2026-11-25). On the newest held-out set it catches 80%
of names and places against a 90% target; that set has only 10 names and
5 places and is synthetic, so the figure is rough. With the rules it meets
the 99% target for secrets and contact details on every set. Keep checking
the preview yourself.

## Set up

Needs Python 3.10 or later, about 1 GB of disk for the libraries and
600 MB of RAM while it runs.

```sh
python3 -m venv ~/.strgl/ner-venv
~/.strgl/ner-venv/bin/pip install transformers torch --extra-index-url https://download.pytorch.org/whl/cpu
```

Then pass the helper to the collector, or set `STRGL_NER_HELPER` once:

```sh
strgl-collector preview -project NAME -ner "$HOME/.strgl/ner-venv/bin/python -I /path/to/collector/ner/strgl_ner.py"
```

The first run downloads the model (`dslim/distilbert-NER` by default;
`--model ID` picks another). After that it works offline. Your text is
only passed to the helper process on this device.

If the helper is set but cannot start, nothing is built. If it fails on an
item, that item and every later one are withheld, never sent with the rules
alone. Bundles record the model, as in `redact-v1.2+ner:dslim/distilbert-NER`.

## Protocol

One JSON object per line. The helper first prints `{"model": "ID"}`, then
answers each `{"text": "..."}` with
`{"entities": [{"type": "PERSON", "text": "Grace Lee"}]}`, or
`{"error": "..."}`. Types must be upper-case letters and underscores; any
other type is replaced with `NAME`. Any program that speaks this can
replace the reference helper.

## Checking a model

```sh
cd collector
STRGL_NER_HELPER="python3 -I ner/strgl_ner.py --model ID" go test -count=1 -run TestRedactionRecall -v .
```

With a helper set, the test also requires 99% recall for secrets and
contact details and 90% for names, schools and places on every corpus,
and at least 95% of the harmless text to survive.

## Results, 2026-11-25

Three corpora written by us: `redact_eval.json` (development),
`redact_eval_holdout.json` (seen once during model selection; one password
rule was fixed after it) and `redact_eval_holdout2.json` (written after
that, used only to compare models, never to tune rules).

| Model | Names, dev / holdout / holdout2 | Places, holdout2 | ms per item, peak RAM |
| --- | --- | --- | --- |
| rules only (`redact-v1.2`) | 71% / 75% / 60% | 20% | under 1 ms |
| `dslim/distilbert-NER` | 94% / 100% / 80% | 80% | 104 ms, 623 MB |
| `dslim/bert-base-NER` | 94% / 100% / 70% | 100% | 193 ms, 780 MB |
| `iiiorg/piiranha-v1-detect-personal-information` | 84% / 92% / 70% | 100% | not measured |

Timings are one CPU thread on an ARM64 cloud machine with short items, not
an old laptop. Misses are mostly lower-case or single first names
("Leila", a name inside a code string). holdout2 has only 10 names, so one
name moves its figure by 10 points. `redact-v1.2` added rules for UK
mobile and landline numbers and postcodes, which `holdout2` (written
before those rules) had exposed; they now pass on all three sets. The
figures above were measured with `redact-v1.1`; the name and place
figures for distilbert are the same with `redact-v1.2`.
