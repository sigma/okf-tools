package gdocs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sigma/okf-tools/internal/publish"
)

// appPropKeyDoc and appPropKeyState are the private Drive properties that make a
// re-run find the same destination. Title matching was rejected: a human renaming
// the document would orphan it and the next run would create a second one (#151).
const (
	appPropKeyDoc   = "okf"
	appPropKeyState = "okfstate"
)

// nodeState is one node's persisted provenance.
//
// BOTH hashes are stored. #153 concluded one would do, reasoning that properties
// render as content here so a property hash gates nothing — but Generation emits
// SetProperties whenever the scan reports no property hash (graph/generate.go),
// so dropping it makes every re-run rewrite every tab and destroys the near-noop
// property. The 30-property ceiling that motivated collapsing to one hash applied
// to Drive appProperties, and does not apply to this sidecar.
type nodeState struct {
	// Tab is the node's server-assigned tabId. It is state, not a derivation: a
	// tabId cannot be computed from the source path (#147).
	Tab string `json:"tab"`
	// Hash is the node's content hash, and PropHash its property hash.
	Hash     publish.Hash `json:"hash,omitempty"`
	PropHash publish.Hash `json:"propHash,omitempty"`
}

// sidecar is the state file's whole shape.
//
// It lives in a separate Drive file rather than in the document, because
// NotebookLM ingests every tab as one source (#148) — a provenance tab would put
// machine state into the model's context and a reader's view. Drive
// appProperties were rejected for the opposite reason: 30 properties per app caps
// a document at ten pages (#153).
type sidecar struct {
	// Version allows the format to change without misreading old state.
	Version int `json:"version"`
	// Nodes is per-node provenance keyed by bundle-relative path.
	Nodes map[string]nodeState `json:"nodes"`
}

// Provision shapes the destination before any write: it finds or creates the
// document and its sidecar, and is idempotent — an already-provisioned
// destination produces no writes beyond the two lookups.
func (b *Backend) Provision(ctx context.Context) error {
	key := b.selectionKey()

	// Resolving here rather than in New keeps New free of I/O, and this is the
	// first request of the run either way. The fan-out builds one backend per
	// area, so this costs one extra READ per selection — cheap against the 60
	// writes per minute that actually bind (#156).
	loc, err := b.c.resolveLocation(ctx, b.cfg.DriveID)
	if err != nil {
		return fmt.Errorf("gdocs: provision: resolve destination: %w", err)
	}

	if err := b.noticeSuperseded(ctx, loc); err != nil {
		return fmt.Errorf("gdocs: provision: %w", err)
	}

	doc, err := b.c.findByAppProperty(ctx, loc, appPropKeyDoc, key)
	if err != nil {
		return fmt.Errorf("gdocs: provision: find document: %w", err)
	}
	// A dry run that CREATED the destination would leave an empty document behind —
	// the opposite of what the flag promises. Provision is therefore find-only here,
	// and the scan reports an empty destination so the dump shows a first publish.
	if doc == nil && b.cfg.DryRunWriter != nil {
		b.mu.Lock()
		b.docID, b.stateID, b.missing = "would-create-document", "", true
		b.mu.Unlock()
		fmt.Fprintf(b.cfg.DryRunWriter,
			"would create a document for selection %q in %s\n", key, loc)
		return nil
	}
	if doc == nil {
		doc, err = b.c.createFile(ctx, driveFile{
			Name:          b.docTitle(),
			MimeType:      mimeDoc,
			Parents:       []string{loc.parent},
			AppProperties: map[string]string{appPropKeyDoc: key},
		})
		if err != nil {
			return fmt.Errorf("gdocs: provision: create document: %w", err)
		}
		if b.cfg.Parts > 0 {
			b.notify("created document %s for part %d of %d of %q; add it to any NotebookLM notebook that loads the area",
				doc.ID, b.cfg.Part, b.cfg.Parts, b.cfg.Selection)
		}
	}

	state, err := b.c.findByAppProperty(ctx, loc, appPropKeyState, key)
	if err != nil {
		return fmt.Errorf("gdocs: provision: find state file: %w", err)
	}
	if state == nil {
		state, err = b.c.createFile(ctx, driveFile{
			Name:          b.stateFileName(),
			MimeType:      mimeJSON,
			Parents:       []string{loc.parent},
			AppProperties: map[string]string{appPropKeyState: key},
		})
		if err != nil {
			return fmt.Errorf("gdocs: provision: create state file: %w", err)
		}
	}

	b.mu.Lock()
	b.docID, b.stateID = doc.ID, state.ID
	b.mu.Unlock()
	return nil
}

// noticeSuperseded reports the documents an earlier split of this selection
// published into and this run no longer writes to (#216). A stale part still
// feeds its old pages to any notebook holding it, so it must not go unsaid. It
// is reported rather than trashed: removing a notebook's source is the
// operator's call, and a trashed document is no longer found here.
//
// The first part looks for the unsplit document, and the last part (or the
// unsplit document, part 0) for every part after it — the two ways the count can
// have changed.
func (b *Backend) noticeSuperseded(ctx context.Context, loc driveLocation) error {
	var probe []int
	if b.cfg.Part == 1 {
		probe = append(probe, 0)
	}
	if b.cfg.Part == b.cfg.Parts {
		probe = append(probe, b.cfg.Parts+1)
	}
	for len(probe) > 0 {
		part := probe[0]
		probe = probe[1:]
		f, err := b.c.findByAppProperty(ctx, loc, appPropKeyDoc, b.keyFor(part))
		if err != nil {
			return fmt.Errorf("look for superseded documents: %w", err)
		}
		if f == nil {
			continue
		}
		b.notify("document %s (%q) is no longer published to: %q now publishes as %d document(s); "+
			"trash it and remove it from any NotebookLM notebook", f.ID, f.Name, b.cfg.Selection, max(b.cfg.Parts, 1))
		if part > 0 {
			probe = append(probe, part+1)
		}
	}
	return nil
}

func (b *Backend) notify(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notices = append(b.notices, fmt.Sprintf(format, args...))
}

// readState loads the sidecar. A missing or unparsable file is an EMPTY state,
// not an error: the recovery path is to re-publish, not to refuse (#153).
func (b *Backend) readState(ctx context.Context) (sidecar, error) {
	s := sidecar{Version: 1, Nodes: map[string]nodeState{}}
	if b.stateID == "" {
		return s, nil
	}
	if b.missing {
		return s, nil
	}
	raw, err := b.c.downloadFile(ctx, b.stateID)
	if err != nil {
		var he *http.Response
		_ = he
		return s, nil //nolint:nilerr // a missing sidecar self-heals; see WriteBack
	}
	if len(raw) == 0 {
		return s, nil
	}
	var parsed sidecar
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return s, nil //nolint:nilerr
	}
	if parsed.Nodes == nil {
		parsed.Nodes = map[string]nodeState{}
	}
	return parsed, nil
}

// WriteBack persists this run's provenance into the sidecar so the NEXT scan can
// hash-skip. It merges rather than replaces: a run touches only some nodes, and
// the untouched ones must keep their state.
func (b *Backend) WriteBack(ctx context.Context, prov publish.Provenance) error {
	if b.cfg.DryRunWriter != nil {
		return nil // a dry run records nothing; there is nothing it wrote
	}
	if len(prov.Nodes) == 0 {
		return nil // an unchanged run writes nothing — the near-noop property
	}
	state, err := b.readState(ctx)
	if err != nil {
		return err
	}

	b.mu.Lock()
	for id, np := range prov.Nodes {
		if _, unclaimed := id.Unclaimed(); unclaimed {
			continue
		}
		rel := id.Rel()
		ns := state.Nodes[rel]
		if tab, ok := b.tabs[rel]; ok {
			ns.Tab = tab
		} else if np.ID != "" {
			ns.Tab = string(np.ID)
		}
		if np.Hash != "" {
			ns.Hash = np.Hash
		}
		if np.PropHash != "" {
			ns.PropHash = np.PropHash
		}
		state.Nodes[rel] = ns
	}
	stateID := b.stateID
	b.mu.Unlock()

	if stateID == "" {
		return fmt.Errorf("gdocs: write-back before provision")
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return b.c.uploadFile(ctx, stateID, raw)
}
