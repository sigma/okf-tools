# Mapping markdown/OKF constructs onto the Google Docs API content model

Research note for [sigma/okf-tools#150](https://github.com/sigma/okf-tools/issues/150).
Every claim below cites a primary source on `developers.google.com`.

## 1. The request vocabulary

All writes go through `documents.batchUpdate`, whose body is an ordered array of
`Request` objects, each a `oneof` over a fixed union of request types
([Request reference](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request)).
The union that matters for content authoring is:

`replaceAllText`, `insertText`, `updateTextStyle`, `createParagraphBullets`,
`deleteParagraphBullets`, `createNamedRange`, `deleteNamedRange`,
`updateParagraphStyle`, `deleteContentRange`, `insertInlineImage`, `insertTable`,
`insertTableRow`, `insertTableColumn`, `deleteTableRow`, `deleteTableColumn`,
`insertPageBreak`, `deletePositionedObject`, `updateTableColumnProperties`,
`updateTableCellStyle`, `updateTableRowStyle`, `replaceImage`, `updateDocumentStyle`,
`mergeTableCells`, `unmergeTableCells`, `createHeader`, `createFooter`,
`createFootnote`, `replaceNamedRangeContent`, `updateSectionStyle`,
`insertSectionBreak`, `deleteHeader`, `deleteFooter`, `pinTableHeaderRows`,
`addDocumentTab`, `deleteTab`, `updateDocumentTabProperties`, `insertPerson`,
`updateNamedStyle`, `insertRichLink`, `insertDate`, `insertComment`,
`addCommentReply`, `updateCommentPost`, `deleteComment`, `deleteCommentReply`,
`acceptSuggestion`, `rejectSuggestion`, `deleteSuggestion`.

Two absences are load-bearing for this design: **there is no `createBookmark`
request and no code-block request**. Both are discussed below.

## 2. Construct → request mapping

| Construct | Request(s) | Key fields | Notes |
| --- | --- | --- | --- |
| Paragraph | `insertText` | `text` (ending `\n`), `location.index` | "Inserting a newline character will implicitly create a new `Paragraph` at that index." ([InsertTextRequest](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#inserttextrequest)) |
| Heading (h1–h6) | `insertText` + `updateParagraphStyle` | `paragraphStyle.namedStyleType` = `HEADING_1`…`HEADING_6`; `fields: "namedStyleType"` | Enum also has `TITLE`, `SUBTITLE`, `NORMAL_TEXT` ([NamedStyleType](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#namedstyletype)) |
| Bold / italic | `updateTextStyle` | `textStyle.bold`, `textStyle.italic`, `range`, `fields` | "Whether or not the text is rendered as bold." ([TextStyle](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#textstyle)) |
| Inline code | `updateTextStyle` | `textStyle.weightedFontFamily.fontFamily` (e.g. `Courier New`) + `textStyle.backgroundColor` | No native inline-code style; degraded rendering |
| Hyperlink | `updateTextStyle` | `textStyle.link.url`, `fields: "link"` | `Link.url` = "An external URL." ([Link](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#link)) |
| Bulleted list | `insertText` + `createParagraphBullets` | `range`, `bulletPreset` = `BULLET_DISC_CIRCLE_SQUARE` | |
| Numbered list | `insertText` + `createParagraphBullets` | `bulletPreset` = `NUMBERED_DECIMAL_ALPHA_ROMAN` / `NUMBERED_DECIMAL_NESTED` | |
| List **nesting** | as above | leading `\t` characters in the inserted text | The request "count[s] leading tabs in front of each paragraph" to derive nesting level, and removes them ([CreateParagraphBulletsRequest](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#createparagraphbulletsrequest)); read back as `Paragraph.bullet.nestingLevel` / `listId` |
| Remove list formatting | `deleteParagraphBullets` | `range` | |
| Table | `insertTable` (+ `insertText` per cell, `updateTableCellStyle`, `pinTableHeaderRows`) | `rows`, `columns`, `location` | "A newline character will be inserted before the inserted table." ([InsertTableRequest](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#inserttablerequest)) |
| Image | `insertInlineImage` | `uri`, `location`, `objectSize` | "Images must be less than 50MB in size, cannot exceed 25 megapixels, and must be in one of PNG, JPEG, or GIF format." ([InsertInlineImageRequest](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#insertinlineimagerequest)) — the URI must be publicly fetchable |
| Block quote | `updateParagraphStyle` | `indentStart`, `indentFirstLine`, `borderLeft` (a `ParagraphBorder`) | No native blockquote; degraded |
| Horizontal rule | `updateParagraphStyle` on an empty paragraph | `borderBottom` (`ParagraphBorder`) | No native HR request in the union |
| Code block | `insertText` + `updateTextStyle` + `updateParagraphStyle` | `weightedFontFamily` (monospace) + `shading.backgroundColor` + `borderLeft`/`borderBottom` | **No native code-block request.** `Shading` is "The background color of text in a paragraph or table cell." ([ParagraphStyle](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#paragraphstyle)) |
| Page break | `insertPageBreak` | `location` | |
| Section break | `insertSectionBreak` | `location`, `sectionType` | |
| Footnote | `createFootnote` | `location` | Creates a new `footnote` segment |
| Template fill | `replaceAllText` / `replaceNamedRangeContent` | — | Defaults to **all tabs** if no tab is given |

`updateTextStyle` and `updateParagraphStyle` both require a `fields` mask: "At
least one field must be specified" and "a single `*` can be used as short-hand for
listing every field."

## 3. Bookmarks and in-document anchors

- **A bookmark cannot be created through the Docs REST API.** No `createBookmark`
  request exists in the `Request` union
  ([Request reference](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request)).
  Bookmark creation is only available in Apps Script, via
  `DocumentTab.addBookmark(position)` — "Adds a `Bookmark` at the given `Position`"
  ([DocumentTab](https://developers.google.com/apps-script/reference/document/document-tab)).
- **A `Link` can nevertheless target a bookmark or a heading in the same document.**
  `Link` has `url`, `tabId` ("The ID of a tab in this document"), `bookmark`
  (`BookmarkLink`) and `heading` (`HeadingLink`), plus the legacy flat `bookmarkId`
  and `headingId` ([Link](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#link)).
- **Tab-specific targeting works.** `BookmarkLink` and `HeadingLink` each carry an
  `id` *and* a `tabId` — "The ID of the tab containing this bookmark" / "…this
  heading" — so a link can point at an anchor in a *specific* tab.
- **Heading IDs are read-only.** `ParagraphStyle.headingId`: "The heading ID of the
  paragraph. If empty, then this paragraph is not a heading. This property is
  read-only."
  ([ParagraphStyle](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#paragraphstyle)).

**Consequence for a glossary-anchor design:** the only API-creatable in-document
anchor is a **heading**, and its ID is assigned by the server. The workable pattern
is two passes: (1) `batchUpdate` inserting the headings, (2) `documents.get` to
harvest each `paragraphStyle.headingId`, (3) a second `batchUpdate` issuing
`updateTextStyle` with `link.heading = {id, tabId}` on every reference. A one-shot
"insert anchors and links in a single batch" is not possible. `createNamedRange` is
*not* a substitute — named ranges are not link targets in the `Link` union.

## 4. Index arithmetic

- Indexes are zero-based offsets **relative to the enclosing segment** (body,
  header, footer, footnote); most body elements carry `startIndex`/`endIndex`
  ([Structure](https://developers.google.com/workspace/docs/api/concepts/structure)).
- "Indexes are measured in UTF-16 code units. This means surrogate pairs consume
  two indexes." (an emoji occupies two).
- `SectionBreak`, `Table` and `Paragraph` do not carry indexes themselves; "their
  enclosing `StructuralElement` has these fields."
- Insertions shift everything after them: "Each insertion increments all the
  higher-numbered indexes by the size of the inserted text."
  ([Insert, delete and move text](https://developers.google.com/workspace/docs/api/how-tos/move-text))
- **The documented technique for building a body in one `batchUpdate`** is to write
  back-to-front: "To avoid having to precalculate these offset changes, order your
  insertions to 'write backwards': do the insertion at the highest-numbered index
  first, working your way towards the beginning with each insertion," so that "each
  write's offsets are unaffected by the preceding ones."

A practical corollary for a markdown renderer: compute the whole target text
locally, then emit requests in descending index order — or, simpler, emit one large
`insertText` for the plain text and then style it with ranges computed against the
now-known final layout (styling requests do not shift indexes).

## 5. Replacing / emptying content

`deleteContentRange` takes a `range` and refuses several deletions
([DeleteContentRangeRequest](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#deletecontentrangerequest)).
It cannot delete:

- "the last newline character of a `Body`, `Header`, `Footer`, `Footnote`,
  `TableCell` or `TableOfContents`"
- "one code unit of a surrogate pair"
- "the start or end of a `Table`, `TableOfContents` or `Equation` without deleting
  the entire element"
- "the newline character before a `Table`, `TableOfContents` or `SectionBreak`
  without deleting the element"
- "individual rows or cells of a table. Deleting the content within a table cell is
  allowed"

**The final-newline caveat is the one that bites.** To empty a body you
`documents.get` the segment, take the body's last structural element `endIndex`, and
delete `[1, endIndex - 1)`. Index 1 is the first editable position (index 0 is not
addressable for content), and `endIndex - 1` excludes the mandatory trailing
newline. Deleting through `endIndex` returns an error. The same holds per tab: pass
`range.tabId` to scope the delete to one tab.

`replaceAllText` is the alternative for templated documents, but note it "will
instead default to applying to all tabs" when no tab criteria are given
([Work with tabs](https://developers.google.com/workspace/docs/api/how-tos/tabs)).

## 6. Tabs

- "Each `Request` includes a way to specify the tabs to apply the update to" —
  typically `Location.tabId` or `Range.tabId`
  ([Work with tabs](https://developers.google.com/workspace/docs/api/how-tos/tabs)).
- "By default, if a tab is not specified, the `Request` will in most cases be
  applied to the first tab in the document."
- Exceptions: `ReplaceAllTextRequest`, `DeleteNamedRangeRequest` and
  `ReplaceNamedRangeContentRequest` "will instead default to applying to all tabs."
- `link.tabId` "exposes internal links to tabs."
- Tabs are created/removed with `addDocumentTab` / `deleteTab` /
  `updateDocumentTabProperties`.

## 7. Atomicity and limits

- **Atomicity:** "Each request is validated before being applied. If any request is
  not valid, then the entire request will fail and nothing will be applied."
  Requests are applied in order, atomically
  ([documents.batchUpdate](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/batchUpdate)).
- **Optimistic concurrency:** `writeControl.requiredRevisionId` fails with 400 if the
  document has moved on; `targetRevisionId` merges server-side.
- **Request count / payload size:** *not documented.* The `batchUpdate` reference
  states no maximum number of requests, and the limits page lists no payload cap
  ([Usage limits](https://developers.google.com/workspace/docs/api/limits)). Treat
  any batch-size ceiling as undiscovered rather than absent, and chunk defensively.
- **Quotas** ([Usage limits](https://developers.google.com/workspace/docs/api/limits)):

  | Quota | Per minute per project | Per minute per user per project |
  | --- | --- | --- |
  | Read requests | 3000 | 300 |
  | Write requests | 600 | 60 |

  Exceeding a quota returns `429: Too many requests`; the guidance is exponential
  backoff with a `maximum_backoff` of "32 or 64 seconds". Standard use is currently
  free, but the page notes charges are "planned to incur charges to your Google
  Cloud billing account later in 2026."

The **60 writes/minute/user** figure is the binding constraint: one `batchUpdate`
call is one write, so batching aggressively is not just an index-drift convenience,
it is the quota strategy.

## 8. Code blocks

There is **no native code-block construct** in the Docs API — no request in the
`Request` union produces one, and `ParagraphStyle` has no code-block property
([Request reference](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request),
[ParagraphStyle](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#paragraphstyle)).

Best degraded rendering, all in one `batchUpdate`:

1. `insertText` with the code body (preserve newlines; each line becomes a paragraph).
2. `updateTextStyle` with `textStyle.weightedFontFamily = {fontFamily: "Courier New"}`
   over the range.
3. `updateParagraphStyle` with `paragraphStyle.shading.backgroundColor` for the grey
   block, plus `indentStart` and optionally `borderLeft`/`borderTop`/`borderBottom`
   for the frame, `fields: "shading,indentStart,borderLeft,..."`.

An alternative is a single-cell `insertTable` with `updateTableCellStyle`
`backgroundColor`, which gives a real bounding box, at the cost of a much fiddlier
index model (tables cannot have their rows/cells partially deleted).

## 9. Alternative path: import markdown via Drive

The Docs API itself has no markdown ingestion. **Drive does.** `files.create` with a
media upload converts on the way in: "To convert a file to a specific Google
Workspace file type, specify the Google Workspace `mimeType` when creating the
file", and the supported-import-formats table lists "Microsoft Word, OpenDocument
Text, HTML, RTF, plain text, **Markdown**" → Google Docs
([Upload file data](https://developers.google.com/workspace/drive/api/guides/manage-uploads)).
The documented sample is literally a markdown upload:

```js
const fileMetadata = {name: 'My Report', mimeType: 'application/vnd.google-apps.document'};
const media = {mimeType: 'text/markdown', body: fs.createReadStream('files/report.md')};
const file = await service.files.create({requestBody: fileMetadata, media, fields: 'id'});
```

The reverse direction is also supported: `text/markdown` (`.md`) is a documented
export MIME type for Google Docs
([Export MIME types](https://developers.google.com/workspace/drive/api/guides/ref-export-formats)).

### Comparison

| | `batchUpdate` synthesis | Drive markdown import |
| --- | --- | --- |
| Fidelity | Exactly what you construct | Whatever Google's converter does; not specified |
| Effort | High — index arithmetic, per-construct mapping | Trivial — one upload |
| Granularity | Can update part of a document/tab in place | Whole-file; creates or replaces a file |
| Tabs | Can target a specific tab | No documented control over tab placement |
| Anchors/links | Full control (modulo §3) | Converter-defined |
| Quota | 1 write per batch, 60/min/user | 1 Drive write per file |
| Round-trip | — | `files.export` back to `text/markdown` |

**Recommendation:** Drive markdown import is the right first implementation if the
unit of publication is a whole document and the fidelity of Google's converter is
acceptable — it eliminates the entire index-arithmetic surface. The `batchUpdate`
path is required as soon as the tool needs (a) incremental updates to an existing
document, (b) per-tab targeting, or (c) precise control of in-document anchors and
links, which is exactly what a glossary-anchor design needs. A hybrid is plausible:
import via Drive for the bulk content, then a follow-up `documents.get` +
`batchUpdate` pass to harvest `headingId`s and install cross-reference links.

## Sources

- [Method: documents.batchUpdate](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/batchUpdate)
- [Request (union of all request types)](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request)
- [REST Resource: documents (Link, TextStyle, ParagraphStyle, NamedStyleType, Bullet)](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents)
- [Structure of a Google Docs document](https://developers.google.com/workspace/docs/api/concepts/structure)
- [Insert, delete, and move text](https://developers.google.com/workspace/docs/api/how-tos/move-text)
- [Work with tabs](https://developers.google.com/workspace/docs/api/how-tos/tabs)
- [Usage limits](https://developers.google.com/workspace/docs/api/limits)
- [Drive: Upload file data (import formats)](https://developers.google.com/workspace/drive/api/guides/manage-uploads)
- [Drive: Export MIME types](https://developers.google.com/workspace/drive/api/guides/ref-export-formats)
- [Apps Script: DocumentTab.addBookmark](https://developers.google.com/apps-script/reference/document/document-tab)
