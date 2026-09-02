# What NotebookLM ingests from a Google Doc

Research note for [#148](https://github.com/sigma/okf-tools/issues/148). Question:
if we render an OKF bundle into a multi-tab Google Doc so NotebookLM can ingest it,
what does NotebookLM actually read — structure or flat text, one tab or all tabs?

**Retrieved:** 2026-09-02. **Product naming:** Google renamed NotebookLM to
**Gemini Notebook**; the help centre still serves both `support.google.com/notebooklm/…`
and `support.google.com/gemininotebook/…` for the same article IDs, and the article
body now says "Gemini Notebook" throughout
([rename announcement](https://blog.google/innovation-and-ai/products/gemini-notebook/notebooklm-gemini-notebook/)).
Quotes below are verbatim from the live help pages.

Primary sources used:

- **SRC-A** — [Add or discover new sources for your notebook (Computer)](https://support.google.com/notebooklm/answer/16215270?hl=en&co=GENIE.Platform%3DDesktop)
- **SRC-B** — [Frequently asked questions](https://support.google.com/notebooklm/answer/16269187?hl=en)
- **SRC-C** — [Upgrade Gemini Notebook](https://support.google.com/notebooklm/answer/16213268?hl=en)
- **SRC-D** — [Google Docs: Use document tabs](https://support.google.com/docs/answer/15499791?hl=en)

## 1. Multi-tab Docs — all tabs, as one source

This is documented, and it is the answer the exporter design needs. SRC-A, under
**Import Google Drive files → Limitations**:

> Gemini Notebook does not import footnotes or comments from Google files.
>
> While Gemini Notebook will pull in data from multiple tabs in Google Docs and
> Google Sheets as one source.

(The second sentence is grammatically truncated in Google's own copy — it reads as a
dangling "While …". The substantive claim, *multiple tabs → one source*, is
unambiguous and appears identically on the `notebooklm`, `gemininotebook`, and `en-GB`
renderings of the article.)

So a multi-tab Doc is **not** truncated to the first/active tab, and it consumes
**one** of the notebook's source slots rather than one per tab. That is exactly the
property the exporter is betting on.

### Sub-tabs: undocumented

Google Docs supports nesting — SRC-D: *"You can nest up to 3 tab levels."* SRC-A says
nothing about whether content in **sub-tabs** is ingested. It refers only to "multiple
tabs".

⚠️ **Secondary, unverified:** several third-party write-ups assert that content in
sub-tabs is *not* imported. That claim does not appear in any primary Google source I
could find, and the truncated "While …" sentence in SRC-A looks like the remains of a
longer sentence that may once have carried a sub-tab caveat. The Wayback Machine was
offline at time of writing, so I could not diff an earlier revision to confirm. **Treat
sub-tab ingestion as unverified.** Recommended posture for the exporter: emit a **flat,
single-level tab structure**, which is documented to work, rather than relying on
nesting, which is not.

## 2. Structure vs plain text

No primary source states that a Doc is flattened to plain text, and none states that
headings/lists/tables are preserved as structure. What the docs *do* say (SRC-A):

> The source content may appear differently in the Gemini Notebook viewer than the
> original file to enable analysis and understanding of the source information. This
> does not change the formatting of the original source file.

That is an explicit warning that the ingested representation is a *re-rendering*, not a
fidelity-preserving copy — but it stops short of saying what survives.

Two documented data points bound the answer:

- **Footnotes and comments are dropped** from Google files (SRC-A, quoted above).
  Anything the exporter puts in a footnote or a comment is lost.
- **For web-URL sources**, SRC-A is explicit: *"Only the text content of the given HTML
  webpage is scraped for use as a source. Images, embedded videos, or nested webpages
  are not imported."* Note this is scoped to the **web URL** import path, not to Docs.
  Google states the analogous flattening for other paths too — YouTube: *"Only the text
  transcript of the video is imported as a source"*; audio: *"The audio file is
  transcribed at the time of import and its text is saved to use as a new source."*

⚠️ **Inference, not documented:** the consistent pattern across every documented import
path is *reduce the artefact to its text*. Images are a **separate, first-class source
type** in NotebookLM (SRC-A lists avif/bmp/gif/…/webp as source types), which suggests
images are handled as their own sources rather than extracted from inside a Doc — but
SRC-A never says whether an image *embedded in a Doc* is read or skipped.

**Design consequence.** Presentational fidelity work — code-block shading, background
colours, font choices, borders — has **no documented mechanism by which it reaches the
model**, and the "may appear differently in the viewer" warning cuts against it. Spend
effort instead on things that survive any text reduction: explicit heading text,
literal section labels, tables whose meaning is carried by their cell *text* rather than
their layout, and prose that names its own structure. Do not put load-bearing content in
footnotes or comments.

## 3. Size and count limits

Per source (SRC-A):

> Each source can contain up to 500,000 words or up to 200MB for uploaded files. You can
> include up to 50 sources (for Free users).

SRC-B restates it and adds the page-count point:

> The current limit is 500,000 words per source or up to 200MB for local uploads.
> There's no page limit.

SRC-B also lists the failure modes: import fails if *"It exceeds the allowed 500,000
word limit per source"*, if *"The file size exceeds the allowed 200MB limit"*, or if
*"Your original PDF file is copy-protected."*

Sources **per notebook**, by tier (SRC-C):

| Tier | Notebooks | Sources per notebook |
|---|---|---|
| Standard (free) | 100/user | 50/notebook |
| Plus | 200/user | 100/notebook |
| Pro | 500/user | 300/notebook |
| Ultra (20 TB) | 500/user | 500/notebook |
| Ultra (30 TB) | 500/user | 600/notebook |

SRC-C does not restate a per-source word limit per tier; the 500,000-word figure in
SRC-A/SRC-B appears to be global.

Other type-specific caps worth knowing (SRC-A): Google Slides *"up to 100 slides"*;
Google Sheets *"at this time, files are limited to 100k tokens"*. **No analogous
Docs-specific cap is documented** — a Google Doc appears to be governed only by the
500,000-word ceiling. Note the Sheets token cap is far tighter than the Docs limit, so
routing tabular content through a Doc rather than a Sheet is the safer choice.

**Design consequence.** 500,000 words is a very large budget for an OKF bundle. The
binding constraint is far more likely to be the **source count** (50 on the free tier)
than per-source size — which again favours "one Doc, many tabs, one source" over "one
Doc per bundle document".

## 4. Link following

**Not documented for Doc sources.** The only primary statement about following links is
scoped to the web-URL import path (SRC-A): *"Images, embedded videos, or nested webpages
are not imported."*

⚠️ **Inference:** there is no documented crawler anywhere in the ingestion pipeline, and
the one place Google addresses nesting it says nesting is not followed. Treat
cross-references inside an exported Doc as **dead weight for retrieval purposes** — a
link to another document does not pull that document's content into the notebook.

**Design consequence.** If two OKF documents need to be reasoned about together, they
must both be *in the notebook* — either as separate sources or as separate tabs of the
same exported Doc. A link from one to the other buys nothing at ingestion time. Where a
cross-reference matters semantically, render enough inline context (the target's title,
and ideally a one-line summary) that the text alone carries the relationship.

## 5. Re-sync when the Doc changes

Documented, and it is automatic. SRC-A, **Import Google Drive files**:

> Sources imported from Google Drive are auto-updated and will sync every few minutes.
> Changes to your original document will automatically update when you open your
> Notebook. If needed, you can manually update a source by opening the source in
> Notebook and clicking "Click to sync with Google Drive."

So there is both an automatic path (every few minutes / on notebook open) and an
explicit manual one ("Click to sync with Google Drive").

Related access semantics from the same section:

> You can only import files if you have view access or more. If you lose access to a
> file in Google Drive or if the file is deleted, the source will be inaccessible, and
> you will no longer be able to view or interact with the source in your notebook.

and:

> Inactive sources will count towards source limits but will not be referenced
> throughout your notebook.

Also (SRC-A): *"Gemini Notebook can't delete or edit your original files in Drive."*
The sync is strictly one-way, Drive → notebook.

**Design consequence.** The exporter can be **idempotent and re-runnable**: overwrite
the same Doc in place on each export and the notebook picks the change up without any
user action. Do *not* delete-and-recreate the Doc — a new file ID orphans the existing
source, which then still counts against the source limit while contributing nothing.
Stable file identity is a hard requirement of the exporter design.

## Summary for the exporter

| Question | Answer | Confidence |
|---|---|---|
| All tabs of a multi-tab Doc ingested? | **Yes**, as one source | Documented (SRC-A) |
| Sub-tabs (nested) ingested? | **Unknown** | ⚠️ Undocumented; use flat tabs |
| Structure preserved, or flattened? | Text-oriented; re-rendered in viewer; footnotes/comments dropped | Partly documented (SRC-A); flattening is inference |
| Presentational fidelity worth building? | **No** — no documented path to the model | Inference |
| Per-source limit | 500,000 words / 200MB; no page limit; no Docs-specific cap | Documented (SRC-A, SRC-B) |
| Sources per notebook | 50 free → 600 Ultra | Documented (SRC-C) |
| Links followed? | **No** — assume not | ⚠️ Documented only for web URLs (SRC-A) |
| Re-sync on edit? | **Yes**, automatic every few minutes, plus manual button | Documented (SRC-A) |

## Open questions worth an empirical check

The docs are silent on these and they are cheap to test with one throwaway Doc and one
notebook:

1. **Sub-tabs.** Build a Doc with a level-2 and a level-3 sub-tab containing a unique
   nonce string, import it, and ask the notebook for the nonce. This is the single
   highest-value experiment — it decides whether the exporter's tab tree can be nested.
2. **Tab titles.** Are tab names carried into the source text as headings, or discarded?
   Determines whether the exporter must *also* emit an in-body `# Title` per tab.
3. **Tables.** Does a Docs table survive as rows/columns the model can read across, or
   collapse into a run of cell text? Determines whether tabular OKF content should be
   rendered as a Docs table at all, versus as labelled prose.
4. **Embedded images.** Read, or silently dropped, when inside a Doc rather than
   uploaded as their own source?
