package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/sigma/okf-tools/internal/publish"
)

// Banner is the data a generated-page disclaimer banner needs, threaded into the
// pure planner from the bins as data (sigma/ideas ADR-0015): the notice Text shown
// on every mirrored page, and the source-repo web coordinates (BaseURL, Ref) its
// deep-link targets. The planner reads no environment — it renders whatever it is
// handed — which keeps its output deterministic and portable. A nil *Banner means
// no banner is injected. Resolution of BaseURL/Ref lives in internal/publish/source.
type Banner struct {
	// Text is the disclaimer notice, rendered as the banner block's visible label.
	Text string
	// BaseURL is the source repo's web base (e.g. https://github.com/sigma/ideas).
	BaseURL string
	// Ref is the branch the /edit/ deep-link targets (e.g. main).
	Ref string
}

// editURL is the GitHub web-editor deep-link for a page's source file:
// <base>/edit/<ref>/<rel>. rel is the bundle-relative, forward-slash source path.
func (bn *Banner) editURL(rel string) string {
	return strings.TrimRight(bn.BaseURL, "/") + "/edit/" + bn.Ref + "/" + rel
}

// block renders the banner as the neutral Quote block a page carries at block-0:
// one inline run whose visible text is the notice and whose link targets the
// page's source file. ADR-0015 fixed the shape as a single quote block (a callout
// was rejected: it is not in the block union and the prominence gain did not
// justify net-new plumbing).
func (bn *Banner) block(rel string) publish.Block {
	return publish.Block{
		Content: BlockContent{
			Kind:    Quote,
			Inlines: []Inline{{Text: bn.Text, URL: bn.editURL(rel)}},
		},
	}
}

// hash folds the banner's rendered text and deep-link into an existing content
// hash so a banner copy or source-URL change re-publishes the affected pages
// (ADR-0015: the banner rides the page content hash). It is necessary because the
// source-side ContentHash covers only the raw markdown, not the constructed block
// list, so without this fold a banner change would be invisible to change
// detection and would never re-sync. The fold is stable: an unchanged banner over
// an unchanged page yields the same hash, so a steady-state re-run still hash-skips.
func (bn *Banner) hash(base publish.Hash, rel string) publish.Hash {
	sum := sha256.Sum256([]byte(string(base) + "\x00" + bn.Text + "\x00" + bn.editURL(rel)))
	return publish.Hash(hex.EncodeToString(sum[:]))
}
