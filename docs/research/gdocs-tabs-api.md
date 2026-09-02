# Can the Google Docs API create and nest document tabs?

Researched 2026-09-02 for [sigma/okf-tools#147](https://github.com/sigma/okf-tools/issues/147).

**Short answer: yes.** Tabs are fully writable through `documents.batchUpdate`:
create, delete, rename, re-parent and reorder are all documented request types,
and every content-bearing request carries a `tabId` (or a `TabsCriteria`) so
content can be routed into a specific tab.

## Sources

Two primary sources, which disagree in *coverage* (not in fact):

- The REST reference: [`documents/request`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request),
  [`documents`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents),
  [`documents.get`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/get).
- The machine-readable discovery document,
  [`https://docs.googleapis.com/$discovery/rest?version=v1`](https://docs.googleapis.com/$discovery/rest?version=v1),
  revision **20260826**. This is the authority the client libraries are generated
  from; all schema quotes below are verbatim from it and match the reference pages.

The prose guide [Work with tabs](https://developers.google.com/workspace/docs/api/how-tos/tabs)
is **read-oriented**: fetched 2026-09-02, its HTML contains no occurrence of
`AddDocumentTab`, `DeleteTab`, `UpdateDocumentTabProperties`, `parentTabId` or
`nestingLevel`. Do not read that omission as "unsupported" — the reference page
for the same API lists all three requests. Treat the guide as incomplete.

Release history: one entry only, [Docs API release notes](https://developers.google.com/workspace/docs/release-notes),
**July 29, 2024** — "Generally available: You can now create and organize
documents with tabs using the Google Docs API."

## Can `documents.batchUpdate` CREATE a tab?

**Yes — documented.** [`AddDocumentTabRequest`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#AddDocumentTabRequest),
the `addDocumentTab` member of the `Request` union:

> Adds a document tab. When a tab is added at a given index, all subsequent tabs'
> indexes are incremented.

Its single field is `tabProperties` (`TabProperties`) — "The properties of the tab
to add. All properties are optional." The reply is
[`AddDocumentTabResponse`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/response),
which returns "The properties of the newly added tab", i.e. this is how you learn
the new `tabId` — read it out of the matching entry in `BatchUpdateDocumentResponse.replies`.

The full `Request` union in discovery revision 20260826 contains exactly three
tab-lifecycle members: `addDocumentTab`, `deleteTab`, `updateDocumentTabProperties`.

## Nesting

**Yes — documented, and settable via the API.**
[`TabProperties`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#TabProperties):

| field | type | notes (verbatim) |
| --- | --- | --- |
| `tabId` | string | "The immutable ID of the tab." |
| `title` | string | "The user-visible name of the tab." |
| `parentTabId` | string | "Optional. The ID of the parent tab. Empty when the current tab is a root-level tab, which means it doesn't have any parents." |
| `index` | int32 | "The zero-based index of the tab within the parent." |
| `iconEmoji` | string | "Optional. The emoji icon displayed with the tab." Invalid emoji → 400. |
| `nestingLevel` | int32 | **"Output only.** The depth of the tab within the document. Root-level tabs start at 0." |

`parentTabId` and `index` are *not* output-only, so nesting and position are set
by writing them — at creation time via `addDocumentTab.tabProperties`, or later
via `updateDocumentTabProperties`. `nestingLevel` is the derived read-back.

On read, the hierarchy is a tree: [`Document.tabs`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#Document)
is a list of [`Tab`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#Tab),
and `Tab.childTabs` is a recursive `Tab[]` — "The child tabs nested within this
tab." The guide's example path is `document.tabs[2].childTabs[0].childTabs[1].documentTab.body`.

## Rename / reorder / delete

**All three documented.**

- Rename, re-parent, reorder, set icon:
  [`UpdateDocumentTabPropertiesRequest`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#UpdateDocumentTabPropertiesRequest)
  — "Update the properties of a document tab." Fields: `tabProperties` and a
  `fields` field mask ("At least one field must be specified. The root
  `tab_properties` is implied and should not be specified. A single `\"*\"` can be
  used as short-hand for listing every field."). **Note there is no separate
  `tabId` field on this request** — the tab being updated is identified by
  `tabProperties.tabId`.
- Delete: [`DeleteTabRequest`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#DeleteTabRequest)
  — "Deletes a tab. **If the tab has child tabs, they are deleted as well.**"
  Field: `tabId`.

## Writing into a specific tab

**Yes.** Two mechanisms, both documented in the reference:

1. **`tabId` on the location/range types.** `Location`, `EndOfSegmentLocation` and
   `Range` each carry a `tabId`, with identical semantics:

   > The tab that the location is in. When omitted, the request is applied to the
   > first tab. In a document containing a single tab: - If provided, must match
   > the singular tab's ID. - If omitted, the request applies to the singular tab.
   > In a document containing multiple tabs: - If provided, the request applies to
   > the specified tab. - If omitted, the request applies to the first tab in the
   > document.

   Since `insertText`, `deleteContentRange`, `updateTextStyle`,
   `updateParagraphStyle`, `insertTable`, `insertInlineImage`, etc. all take a
   `Location`/`EndOfSegmentLocation`/`Range`, they are all tab-addressable.
   **The omission default is a footgun: an un-tabbed request silently lands in
   tab 0**, not an error.

2. **Direct `tabId` on requests with no location:** `updateDocumentStyle`,
   `updateNamedStyle`, `deleteHeader`, `deleteFooter`, `deletePositionedObject`,
   `replaceImage`.

3. **`TabsCriteria` (`{ tabIds: string[] }`) for multi-tab requests:**
   `replaceAllText`, `replaceNamedRangeContent`, `deleteNamedRange`. Here the
   default is the opposite — "When omitted, the replacement applies to **all**
   tabs."

**Replacing a tab's body wholesale: not found as a single request.** There is no
`replaceBody`/`setTabContent` in the `Request` union. The documented composition is
`deleteContentRange` over the tab's body range followed by `insertText` (both
scoped by `tabId`); or `deleteTab` + `addDocumentTab` to rebuild it. Note
`Body`'s content always ends in a final newline that cannot be deleted, so a
"clear" is `deleteContentRange` over `[1, endIndex-1)`.

## Limits

- **Max tabs per document: 100.** [Docs Editors Help — Use document tabs](https://support.google.com/docs/answer/15499791)
  — "If you can't add more tabs, you might have reached the 100 tabs maximum."
- **Max nesting: 3 levels.** Same page — "You can nest up to 3 tab levels."
  (So `nestingLevel` ∈ {0, 1, 2}.)

Both are **product** limits stated in end-user help, **not** in the API
reference — I found no statement of either in developers.google.com, and no
documented error code for exceeding them. Assume `batchUpdate` returns 400 and
handle it defensively rather than relying on a documented contract.

API request quotas (a separate axis) are at
[Usage limits](https://developers.google.com/workspace/docs/api/limits).

## `includeTabsContent` on `documents.get`

Query parameter, boolean, documented on [`documents.get`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/get):

> Whether to populate the `Document.tabs` field instead of the text content fields
> like `body` and `documentStyle` on `Document`. - When `True`: Document content
> populates in the `Document.tabs` field instead of the text content fields in
> `Document`. - When `False`: The content of the document's first tab populates
> the content fields in `Document` excluding `Document.tabs`. If a document has
> only one tab, then that tab is used to populate the document content.
> `Document.tabs` will be empty.

The guide adds: with `true`, "All of the text fields directly on `document`
(e.g. `document.body`) will be left as empty"; without it, those fields are
"populated with content from the first tab only."

**Operational consequence:** any code that reads `document.body` without passing
`includeTabsContent=true` sees only tab 0 and cannot tell a single-tab document
from a multi-tab one — the other tabs are invisible, not empty. Always pass
`includeTabsContent=true` on reads, and drive off `document.tabs`.

Per-tab content lives in [`DocumentTab`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#DocumentTab):
each tab has its **own** `body`, `documentStyle`, `namedStyles`, `headers`,
`footers`, `footnotes`, `lists`, `namedRanges`, `inlineObjects`,
`positionedObjects`. Indices are per-tab-segment, so an index computed against
one tab is meaningless in another.

## Implications for a multi-page document

Tabs are a genuine API-writable structure, so a page-per-tab shape is viable — no
heading-delimited fallback is needed. Caveats to design around:

- **100 tabs / 3 levels** caps the mapping. A source tree deeper or wider than
  that must be flattened, or fall back to heading-delimited sections within a tab.
- **No content-addressed idempotency.** `tabId` is server-assigned and immutable;
  there is no "create tab with this title if absent". A reconciling writer must
  read the tab tree (`includeTabsContent=true`), match on `title` or on an anchor
  it stores itself, and persist the `tabId` ↔ source mapping — the same
  anchor-recording problem this repo already solves for blocks.
- **Ordering is index arithmetic.** `addDocumentTab` "increments all subsequent
  tabs' indexes", so batching several creates requires computing indices against
  the state *after* earlier requests in the same batch.
- **`deleteTab` cascades to children** — deleting a section tab silently takes its
  subtree with it.
- **Omitted `tabId` defaults to tab 0**, so every content request in a multi-tab
  writer must set it explicitly. Worth enforcing at the type level rather than by
  convention.

## Explicitly not found in the docs

- Any single request that replaces a tab's body wholesale.
- Any documented per-tab limit, nesting limit, or error code on
  developers.google.com (only the end-user help page states 100 / 3).
- Any mention of tab creation, `parentTabId` or `nestingLevel` in the
  [Work with tabs](https://developers.google.com/workspace/docs/api/how-tos/tabs) guide.
- Whether `documents.create` can seed a document with multiple tabs (the
  `Document` request body includes `tabs`, but no doc states it is honoured on
  create; assume it is not, and add tabs in a follow-up `batchUpdate`).
