// Package markdown renders a deliberately small subset of CommonMark.
//
// Raw HTML, links, images, and autolinks are removed. The resulting HTML is
// safe to insert into a page and is intended to be used on both the server and
// the client.
package markdown

import (
	"bytes"
	"fmt"
	"regexp"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
)

var languageClass = regexp.MustCompile(`^language-[a-zA-Z0-9_+-]+$`)

// Renderer converts CommonMark source to sanitized HTML.
type Renderer struct {
	md     goldmark.Markdown
	policy *bluemonday.Policy
}

// New returns a renderer with a fixed allowlist of formatting elements.
func New() *Renderer {
	policy := bluemonday.NewPolicy()
	policy.AllowElements(
		"p", "br", "strong", "em", "del",
		"code", "pre",
		"blockquote", "ul", "ol", "li",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"hr",
	)
	policy.AllowAttrs("class").Matching(languageClass).OnElements("code")

	return &Renderer{
		md:     goldmark.New(),
		policy: policy,
	}
}

// Render parses src as CommonMark and returns sanitized HTML.
func (r *Renderer) Render(src string) (string, error) {
	var buf bytes.Buffer
	if err := r.md.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	return string(r.policy.SanitizeBytes(buf.Bytes())), nil
}
