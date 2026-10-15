# Prepared PDF stationery proof

Tracked in [revisionist_writer #2787](https://github.com/LunaONE/revisionist_writer/issues/2787).
This is an AI-assisted experiment authored with Codex, based on pdfcpu **v0.16.1**,
commit **2f7a89a0a69c117a5252601907ff9f31ac4c70f2**. It is not integrated into the
application or a general-purpose PDF/A converter. Upstream's contribution policy
excludes AI-assisted submissions; this work must not be submitted upstream.

## Decision and mini-goals

**Go for a restricted prepared-stationery pilot; not ready for arbitrary uploads or
production integration.** Original PDF text can be composed without outlining,
rasterization or hiding legal information as artifacts. The proof retains both
PDF/A-2a and PDF/UA-1 on the tested corpus.

1. **Round-trip baseline — passed.** Three-page Typst document survives pdfcpu
   read/write with both profiles, identical text positions and identical rendered
   pixels. `PreserveInfoDict=true` avoids a gratuitous Info/XMP mismatch;
   optimization is disabled in this experiment.
2. **Faithful composition — passed on restricted fixtures.** Original source
   content bytes, embedded font programs, body streams, links and tags survive.
   A second fixture exercises an isolated RGB transparency group, alpha and
   Multiply blending. Both composed variants match an independent qpdf underlay
   on every page at 100 dpi, rendered with Poppler.
3. **Meaningful tags — structural proof passed; human acceptance open.** The
   imported Form XObject gets a unique `StructParents` key per page; new paragraph
   elements point to its MCIDs through MCR dictionaries with `Stm` and `Pg`.
   ParentTree ownership is retained. The two legal paragraphs on each of three
   pages are all exposed. The logical order is the original body followed by
   all six stationery paragraphs. That repetition and placement require a
   product decision and an actual screen-reader audit.
4. **Independent validation — automated gates passed; publication partial.**
   veraPDF 1.30.3 checks both declared profiles on the exact final files. Negative
   controls also prove that conformance alone is insufficient. A local fork,
   sources and evidence are available; GitHub fork creation returned HTTP 403
   because the current token cannot create the fork.

## What the fork changes

`pkg/pdfcpu/stationery.go` adds `ComposePreparedStationery` without modifying the
stock watermark implementation. It reuses pdfcpu's private object migration code,
imports the original resources and page transparency Group, wraps the original
stream in a Form XObject, and connects its actual text to the destination tag tree.
It does not add optional-content layers or an Artifact wrapper. Existing body
structure, metadata and link objects remain in place.

The context-level primitive is deliberately **not** a PDF/A/UA validator. Callers
must independently validate both inputs before composition and the exact saved
output afterwards, withhold output on failure, and discard contexts after an error.
The probe executable is a test driver, not a safe end-user conversion command.
The acceptance runner demonstrates these validation gates.

Supported input contract:

- One already conforming source page, with `Document -> P` structure and direct,
  contiguous integer MCIDs. No source annotations, custom roles or paragraph
  attributes. Document language is preserved; other source Document attributes
  outside the explicit allowlist are refused.
- Source and all target pages have matching media/crop boxes, zero origin and no
  rotation. This is an underlay; callers must reserve space in the body layout.
- Exactly one RGB output intent in each input, with identical decoded ICC bytes.
  This restriction does not attempt color conversion or prove every arbitrary
  resource safe. Input and final conformance validation remain mandatory.
- Flat source and destination ParentTrees, one top-level Document each; destination
  ParentTree Limits are refused. Nested number trees require additional work.
- No encryption, layers, attachments, forms or catalog actions. The restrictions
  are a supported-shape gate, not a comprehensive hostile-PDF security sanitizer.

The primitive operates on prepared, independently validated PDF/A-2a + PDF/UA-1
inputs. It cannot repair untagged uploads, missing font programs, missing Unicode
mappings or arbitrary tag trees. No PDF/UA-2 or PDF 2.0 support is claimed.

## Test documents and controls

`fixtures/*.typ` are synthetic, redistributable source files; corresponding PDFs
are committed so Go tests do not require Typst. They use bundled Libertinus Serif
fonts, German legal details, Unicode (ß, €, accented names), a vector rule, headings,
a list, a table, page numbers and internal/external links. The body has three pages.
No customer material is included. These are representative Typst fixtures, not
exports from the application's actual template pipeline.

`probe` derives the transparency and deliberately invalid variants. Unit tests
refuse untagged content, annotations, rotation, crop/origin differences, destination
size differences, active content, attachments, layers, unsupported structure roles
and attributes, malformed MCID ownership, unsupported ParentTrees, incompatible
output profiles and multiple source pages. The CMYK and differing-profile unit
cases mutate dictionary/profile data to test refusal; they are **not** real-world
CMYK color-conversion tests.

Two especially useful negative controls:

- Omitting stationery entirely still passes both standards, but fails the legal
  text/position gate.
- The stock watermark path marks imported content as an Artifact. Its optional
  content metadata also fails standards checks. A test-only control corrects those
  unrelated metadata fields: **both standards then pass while all legal stationery
  text is absent from the structure tree.** Text extraction alone still succeeds;
  the structure gate rejects this output. This is why a green validator report
  cannot be our only success condition.

Deleting embedded font programs or Unicode mappings fails both validator profiles.
The test driver contains intentionally destructive modes only to generate these
controls. Do not use it as a production tool.

## Reproduce

Requirements: Go 1.26.8 (upstream toolchain), Typst **0.15.0**, veraPDF **1.30.3**
with a Java runtime, qpdf **12.2.0**, Poppler **25.03.0**, Python 3 (stdlib only).
The runner checks Typst/veraPDF versions and compiles fixtures with creation time 0.
Use the same versions for exact pixel comparisons. No browser or application stack
is needed.

The veraPDF installer used was
<https://software.verapdf.org/rel/1.30/verapdf-greenfield-1.30.3-installer.zip>,
SHA-256 `b512bf24945503a33fc10738ecffc1f2dccaaf8593a448e94ee9ac86a9559a59`.
Install its CLI using the release's installer instructions.

From this repository root:

```sh
go test ./pkg/pdfcpu -run TestPreparedStationery -count=1
go vet ./pkg/pdfcpu ./experiments/stationery/probe
TYPST=/path/to/typst VERAPDF=/path/to/verapdf python3 experiments/stationery/check.py
```

The runner generates `out/summary.json`, complete veraPDF reports, PDF variants,
qpdf visual references and Go test output. `out/` is ignored. Representative results
and the machine-readable summary are saved under `evidence/`. Negative PDFs are
clearly named controls and must never be delivered to users.

The full `pkg/pdfcpu` and `pkg/api` test run has one environment failure when run as
root: upstream `TestInspectConfigurationReportsReadOnlyPaths` expects chmod to make
paths unwritable, which root bypasses. It also fails on the unmodified v0.16.1
checkout. The remainder passes with only that test excluded. This is not a claim
that the complete upstream repository test suite was run.

## Remaining acceptance work (tracked in #2787)

- Actual app exports and representative, authorized customer stationery: common
  office/graphics exporters, bitmap/vector logos, nested structures/number trees,
  and real differing color profiles. Identify how much upload preparation is needed.
- Human reading-order and screen-reader review, including the order/repetition of
  legal details. Tagged text is necessary but not sufficient for usable accessibility.
- A fail-closed app wrapper: input validation, supported-shape errors, output
  validation before delivery, time/resource limits, version pinning and regression
  fixtures. This fork adds no upload UI, dependency replacement or production path.
- Assess PDF/UA-2 separately if the application moves to PDF 2.0; current results
  cover only A-2a + UA-1 and do not establish insurer/notary legal requirements.
- Publish the local fork when credentials permit. Avoid upstream submission under
  the current upstream AI-contribution policy.

This establishes that the narrow composition problem is tractable within a short
prototype. It does not establish a three-day production fix for arbitrary uploads.
