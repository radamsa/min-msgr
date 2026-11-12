package markdown

import (
	"strings"
	"testing"
)

func render(t *testing.T, src string) string {
	t.Helper()
	out, err := New().Render(src)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return out
}

func TestRenderAllowedFormatting(t *testing.T) {
	src := "# Title\n\n**bold** *italic* `code`\n\n- one\n- two\n\n> quote\n\n---\n\n```go\nfmt.Println(\"hi\")\n```\n"
	out := render(t, src)
	for _, want := range []string{
		"<h1>Title</h1>", "<strong>bold</strong>", "<em>italic</em>",
		"<code>code</code>", "<ul>", "<li>one</li>", "<blockquote>",
		"<hr>", `<code class="language-go">`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderForbidsLinks(t *testing.T) {
	out := render(t, "[click](https://example.com)")
	if strings.Contains(out, "<a") || strings.Contains(out, "href") {
		t.Fatalf("link survived sanitization:\n%s", out)
	}
	if !strings.Contains(out, "click") {
		t.Errorf("link text was lost:\n%s", out)
	}
}

func TestRenderForbidsImages(t *testing.T) {
	out := render(t, "![alt](https://example.com/image.png)")
	if strings.Contains(out, "<img") || strings.Contains(out, "src=") {
		t.Fatalf("image survived sanitization:\n%s", out)
	}
}

func TestRenderForbidsAutolinks(t *testing.T) {
	out := render(t, "<https://example.com>")
	if strings.Contains(out, "<a") || strings.Contains(out, "href") {
		t.Fatalf("autolink survived sanitization:\n%s", out)
	}
}

func TestRenderForbidsRawHTML(t *testing.T) {
	for _, src := range []string{
		`<script>alert(1)</script>`,
		`<b>bold</b>`,
		`<img src=x onerror=alert(1)>`,
	} {
		out := render(t, src)
		for _, forbidden := range []string{"<script", "<b>", "<img", "onerror", "alert(1)"} {
			if strings.Contains(out, forbidden) {
				t.Errorf("raw HTML %q survived: found %q in %s", src, forbidden, out)
			}
		}
	}
}

func TestRenderEscapesCodeBlocks(t *testing.T) {
	out := render(t, "```\n<script>alert(1)</script>\n```\n")
	if strings.Contains(out, "<script") {
		t.Fatalf("code block was not escaped:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("escaped code missing:\n%s", out)
	}
}

func TestRenderForbidsJavascriptLinks(t *testing.T) {
	out := render(t, "[x](javascript:alert(1))")
	if strings.Contains(out, "javascript:") || strings.Contains(out, "href") {
		t.Fatalf("javascript link survived:\n%s", out)
	}
}

func TestRenderEmpty(t *testing.T) {
	if out := render(t, ""); out != "" {
		t.Errorf("Render(\"\") = %q, want empty", out)
	}
}
