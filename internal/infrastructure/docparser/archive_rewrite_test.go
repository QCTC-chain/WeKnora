package docparser

import (
	"reflect"
	"strings"
	"testing"
)

func TestCollectRelativeImageRefs_MarkdownAndHTML(t *testing.T) {
	md := strings.Join([]string{
		"# Title",
		"",
		"![plain](images/a.png)",
		`![titled](images/b.jpg "Figure 1")`,
		"![angle](<images/c d.png>)",
		"<img src=\"images/e.png\" alt=\"html\">",
		"![remote](https://example.com/f.png)",
		"![data](data:image/png;base64,AAAA)",
		"![provider](resource://AbCdEfGhIjKlMnOpQrStUv)",
		"![anchor](#section)",
		"",
		"```",
		"![in code](images/g.png)",
		"```",
		"",
		"`![inline code](images/h.png)`",
		"  ![indented](images/i.png)",
	}, "\n")

	refs := CollectRelativeImageRefs(md)
	var got []string
	for _, ref := range refs {
		got = append(got, ref.Ref)
	}
	want := []string{
		"images/a.png",
		"images/b.jpg",
		"images/c d.png",
		"images/e.png",
		"images/i.png",
	}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collected[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestCollectRelativeImageRefs_OffsetsTargetThePath(t *testing.T) {
	md := `![titled](images/b.jpg "Figure 1")`
	refs := CollectRelativeImageRefs(md)
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if md[refs[0].Start:refs[0].End] != "images/b.jpg" {
		t.Fatalf("span %q, want images/b.jpg", md[refs[0].Start:refs[0].End])
	}
}

func TestRewriteRelativeImageRefs_ReplacesAndReports(t *testing.T) {
	md := "before ![one](images/a.png) mid ![two](images/b.jpg \"t\") ![gone](images/missing.png) after\n<img src=\"images/a.png\"> tail"
	resolve := func(ref string) (string, bool) {
		switch ref {
		case "images/a.png":
			return "resource://AAAA1111bbbb2222cccc", true
		case "images/b.jpg":
			return "resource://DDDD4444eeee5555ffff", true
		}
		return "", false
	}
	rewritten, unresolved := RewriteRelativeImageRefs(md, resolve)

	want := "before ![one](resource://AAAA1111bbbb2222cccc) mid ![two](resource://DDDD4444eeee5555ffff \"t\") ![gone](images/missing.png) after\n<img src=\"resource://AAAA1111bbbb2222cccc\"> tail"
	if rewritten != want {
		t.Fatalf("rewritten =\n%s\nwant\n%s", rewritten, want)
	}
	if len(unresolved) != 1 || unresolved[0] != "images/missing.png" {
		t.Fatalf("unresolved = %v, want [images/missing.png]", unresolved)
	}
}

func TestRewriteRelativeImageRefs_Idempotent(t *testing.T) {
	md := "![a](images/a.png) and <img src=\"images/b.jpg\">"
	resolve := func(ref string) (string, bool) {
		return "resource://AbCdEfGhIjKlMnOpQrStUv", true
	}
	once, unresolved := RewriteRelativeImageRefs(md, resolve)
	if len(unresolved) != 0 {
		t.Fatalf("first pass unresolved = %v", unresolved)
	}
	twice, unresolved2 := RewriteRelativeImageRefs(once, resolve)
	if twice != once {
		t.Fatalf("second pass changed the document:\n%s\nvs\n%s", twice, once)
	}
	if len(unresolved2) != 0 {
		t.Fatalf("second pass unresolved = %v", unresolved2)
	}
	// The provider-scheme references must be exactly one per original site.
	if got := strings.Count(twice, "resource://AbCdEfGhIjKlMnOpQrStUv"); got != 2 {
		t.Fatalf("want 2 rewritten refs, got %d in %s", got, twice)
	}
}

func TestRewriteRelativeImageRefs_BrokenSyntaxUntouched(t *testing.T) {
	md := "![no close](images/a.png\n![ok](images/b.png)"
	resolve := func(ref string) (string, bool) {
		if ref == "images/b.png" {
			return "resource://AbCdEfGhIjKlMnOpQrStUv", true
		}
		return "", false
	}
	rewritten, _ := RewriteRelativeImageRefs(md, resolve)
	// The unterminated link must survive byte-for-byte.
	if !strings.Contains(rewritten, "![no close](images/a.png\n") {
		t.Fatalf("broken syntax was altered: %q", rewritten)
	}
	if !strings.Contains(rewritten, "![ok](resource://AbCdEfGhIjKlMnOpQrStUv)") {
		t.Fatalf("valid ref not rewritten: %q", rewritten)
	}
}

func TestCollectAndRewriteProviderImageRefs(t *testing.T) {
	md := "前文\n![图](resource://AbCdEfGhIjKlMnOpQrStUv \"标题\")\n" +
		"![远程](https://example.com/a.png)\n" +
		"`![代码内](resource://AbCdEfGhIjKlMnOpQrStUv)`\n" +
		"<img src=\"resource://ZzYyXwWvUtSrQpOnMlKjIk\" alt=\"html\">\n" +
		"![普通](images/local.png)\n"

	refs := CollectProviderImageRefs(md)
	got := make([]string, 0, len(refs))
	for _, ref := range refs {
		got = append(got, ref.Ref)
	}
	want := []string{
		"resource://AbCdEfGhIjKlMnOpQrStUv",
		"resource://ZzYyXwWvUtSrQpOnMlKjIk",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider refs = %v, want %v", got, want)
	}

	resolve := func(ref string) (string, bool) {
		switch ref {
		case "resource://AbCdEfGhIjKlMnOpQrStUv":
			return "images/first.jpg", true
		case "resource://ZzYyXwWvUtSrQpOnMlKjIk":
			return "images/second.png", true
		}
		return "", false
	}
	rewritten, unresolved := RewriteProviderImageRefs(md, resolve)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v", unresolved)
	}
	if !strings.Contains(rewritten, `![图](images/first.jpg "标题")`) {
		t.Fatalf("title not preserved: %q", rewritten)
	}
	if !strings.Contains(rewritten, `<img src="images/second.png" alt="html">`) {
		t.Fatalf("html src not rewritten: %q", rewritten)
	}
	if !strings.Contains(rewritten, "`![代码内](resource://AbCdEfGhIjKlMnOpQrStUv)`") {
		t.Fatalf("code span rewritten: %q", rewritten)
	}
	if !strings.Contains(rewritten, "![远程](https://example.com/a.png)") ||
		!strings.Contains(rewritten, "![普通](images/local.png)") {
		t.Fatalf("non-provider refs changed: %q", rewritten)
	}
	if strings.Count(rewritten, "images/first.jpg") != 1 {
		t.Fatalf("unexpected rewrite count: %q", rewritten)
	}

	// Round trip: an exported archive re-imports and finds every relative
	// reference again — the two restored images plus the one that was always
	// relative. A second archive import then re-matches them from images/.
	_, unresolved2 := RewriteRelativeImageRefs(rewritten, func(string) (string, bool) { return "", false })
	want2 := []string{"images/first.jpg", "images/second.png", "images/local.png"}
	if !reflect.DeepEqual(unresolved2, want2) {
		t.Fatalf("re-import refs = %v, want %v", unresolved2, want2)
	}
}

func TestReferenceStyleImagesCollectedAndRewritten(t *testing.T) {
	md := "前置说明\n\n![全景][panorama]\n" +
		"![细节][]\n" +
		"正文引用 [链接][link-label] 不算图片。\n" +
		"[panorama]: images/pano.jpg \"全景标题\"\n" +
		"[细节]: images/detail.png\n" +
		"[link-label]: https://example.com\n" +
		"[未用]: images/unused.png\n" +
		"```\n[代码内]: images/code.png\n```\n"

	refs := CollectRelativeImageRefs(md)
	got := make([]string, 0, len(refs))
	kinds := make(map[string]ImageRefKind)
	for _, ref := range refs {
		got = append(got, ref.Ref)
		kinds[ref.Ref] = ref.Kind
	}
	want := []string{"images/pano.jpg", "images/detail.png"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	if kinds["images/pano.jpg"] != RefKindReference || kinds["images/detail.png"] != RefKindReference {
		t.Fatalf("kinds = %v, want reference", kinds)
	}

	resolve := func(ref string) (string, bool) {
		if ref == "images/pano.jpg" {
			return "resource://AbCdEfGhIjKlMnOpQrStUv", true
		}
		return "", false
	}
	rewritten, unresolved := RewriteRelativeImageRefs(md, resolve)
	if len(unresolved) != 1 || unresolved[0] != "images/detail.png" {
		t.Fatalf("unresolved = %v", unresolved)
	}
	// The definition (not the usage) is rewritten; the title survives.
	if !strings.Contains(rewritten, "[panorama]: resource://AbCdEfGhIjKlMnOpQrStUv \"全景标题\"") {
		t.Fatalf("definition not rewritten with title: %q", rewritten)
	}
	if !strings.Contains(rewritten, "![全景][panorama]") {
		t.Fatalf("usage was altered: %q", rewritten)
	}
	if strings.Contains(rewritten, "[未用]: resource://") ||
		!strings.Contains(rewritten, "[未用]: images/unused.png") {
		t.Fatalf("unused definition was rewritten: %q", rewritten)
	}
	if strings.Contains(rewritten, "[代码内]: resource://") {
		t.Fatalf("code-block definition was rewritten: %q", rewritten)
	}
	if !strings.Contains(rewritten, "[link-label]: https://example.com") {
		t.Fatalf("link definition changed: %q", rewritten)
	}
}
