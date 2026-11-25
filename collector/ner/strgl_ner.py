# SPDX-License-Identifier: AGPL-3.0-only
"""Reference entity-model helper for strgl-collector's optional NER redaction.

Speaks the line protocol in collector/ner.go: prints {"model": ID} once,
then answers each {"text": ...} line with {"entities": [{"type", "text"}]}.
It runs only on this device; the text it reads never leaves it.

    python3 strgl_ner.py [--model ID] [--threshold 0.5]

Needs `transformers` and a CPU build of `torch` (see collector/ner/README.md).
"""

import argparse
import json
import sys

# Model labels mapped to the collector's placeholder types. Labels not listed
# here (dates, numbers, MISC) are ignored; the rules handle what matters.
LABELS = {
    "PER": "PERSON", "PERSON": "PERSON", "FIRSTNAME": "PERSON", "LASTNAME": "PERSON",
    "MIDDLENAME": "PERSON", "GIVENNAME": "PERSON", "SURNAME": "PERSON", "NAME": "PERSON",
    "PREFIX": None,
    "LOC": "PLACE", "LOCATION": "PLACE", "CITY": "PLACE", "STATE": "PLACE", "COUNTY": "PLACE",
    "COUNTRY": None,
    "STREET": "ADDRESS", "STREETADDRESS": "ADDRESS", "BUILDINGNUMBER": "ADDRESS",
    "BUILDINGNUM": "ADDRESS", "ZIPCODE": "ADDRESS", "ZIP": "ADDRESS", "SECONDARYADDRESS": "ADDRESS",
    # Organisation labels are left out: in coding text they mostly hit
    # libraries and products ("Next.js"), and the rules already catch schools.
    "ORG": None, "ORGANIZATION": None, "COMPANYNAME": None,
    "USERNAME": "HANDLE", "DISPLAYNAME": "HANDLE",
    "EMAIL": "EMAIL", "PHONENUMBER": "PHONE", "TELEPHONENUM": "PHONE", "PHONE": "PHONE",
    "PASSWORD": "SECRET", "DOB": "DATE_OF_BIRTH", "DATEOFBIRTH": "DATE_OF_BIRTH",
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="dslim/distilbert-NER")
    ap.add_argument("--threshold", type=float, default=0.5)
    args = ap.parse_args()

    from transformers import pipeline  # imported late so --help is fast

    ner = pipeline("token-classification", model=args.model, aggregation_strategy="simple", device=-1)
    # Without a stride the pipeline silently drops everything after the
    # model's maximum length (512 tokens), so names late in a long tool
    # output would leak. A stride splits long text into overlapping windows.
    ner_kwargs = {"stride": 64} if ner.tokenizer.is_fast else None
    if ner_kwargs is None:
        sys.exit("strgl_ner: the model needs a fast tokenizer to read long text")
    print(json.dumps({"model": args.model}), flush=True)
    for line in sys.stdin:
        try:
            text = json.loads(line)["text"]
            entities = []
            for e in ner(text, **ner_kwargs):
                label = e["entity_group"].upper().replace("B-", "").replace("I-", "").replace("_", "")
                kind = LABELS.get(label)
                value = text[e["start"]:e["end"]].strip()
                if kind and e["score"] >= args.threshold and value:
                    entities.append({"type": kind, "text": value})
            print(json.dumps({"entities": entities}), flush=True)
        except Exception as err:  # report, so the collector withholds the item
            print(json.dumps({"error": f"{type(err).__name__}: {err}"}), flush=True)


if __name__ == "__main__":
    main()
