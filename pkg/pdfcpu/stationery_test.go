package pdfcpu_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

func stationeryContext(t *testing.T, name string) *model.Context {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "experiments", "stationery", "fixtures", name+".pdf"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	conf := model.NewDefaultConfiguration()
	conf.PreserveInfoDict = true
	conf.Optimize = false
	conf.OptimizeBeforeWriting = false
	ctx, err := api.ReadAndValidate(t.Context(), f, conf)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
func stationeryDict(t *testing.T, ctx *model.Context, o types.Object) types.Dict {
	t.Helper()
	d, err := ctx.DereferenceDict(o)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func stationeryStream(t *testing.T, ctx *model.Context, o types.Object) *types.StreamDict {
	t.Helper()
	d, _, err := ctx.DereferenceStreamDict(o)
	if err != nil || d == nil {
		t.Fatalf("stream: %v", err)
	}
	if err = d.Decode(); err != nil {
		t.Fatal(err)
	}
	return d
}
func stationeryPage(t *testing.T, ctx *model.Context, n int) types.Dict {
	t.Helper()
	d, _, _, err := ctx.PageDict(t.Context(), n, false)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func stationeryDoc(t *testing.T, ctx *model.Context) types.Dict {
	t.Helper()
	root := stationeryDict(t, ctx, ctx.RootDict["StructTreeRoot"])
	a, err := ctx.DereferenceArray(root["K"])
	if err != nil {
		t.Fatal(err)
	}
	return stationeryDict(t, ctx, a[0])
}
func stationeryFonts(t *testing.T, ctx *model.Context) map[[32]byte]bool {
	t.Helper()
	out := map[[32]byte]bool{}
	for _, entry := range ctx.Table {
		d, ok := entry.Object.(types.Dict)
		if !ok {
			continue
		}
		for _, key := range []string{"FontFile", "FontFile2", "FontFile3"} {
			if o := d[key]; o != nil {
				out[sha256.Sum256(stationeryStream(t, ctx, o).Content)] = true
			}
		}
	}
	return out
}

func TestPreparedStationeryPreservesContentAndStructure(t *testing.T) {
	dst, src := stationeryContext(t, "body"), stationeryContext(t, "letterhead")
	before := make([][]byte, dst.PageCount)
	annots := make([]types.Object, dst.PageCount)
	for n := 1; n <= dst.PageCount; n++ {
		p := stationeryPage(t, dst, n)
		b, e := dst.PageContent(p, n)
		if e != nil {
			t.Fatal(e)
		}
		before[n-1] = bytes.Clone(b)
		if p["Annots"] != nil {
			annots[n-1] = p["Annots"].Clone()
		}
	}
	metadata := bytes.Clone(stationeryStream(t, dst, dst.RootDict["Metadata"]).Content)
	originalKids := stationeryDoc(t, dst)["K"].Clone()
	stationeryDoc(t, src)["Lang"] = types.StringLiteral("de-DE")
	sourceContent, err := src.PageContent(stationeryPage(t, src, 1), 1)
	if err != nil {
		t.Fatal(err)
	}
	fonts := stationeryFonts(t, src)
	if len(fonts) == 0 {
		t.Fatal("fixture has no fonts")
	}
	// Exercise the page transparency-group path independently of the CLI fixtures.
	group := types.Dict{"S": types.Name("Transparency"), "CS": types.Name("DeviceRGB"), "I": types.Boolean(true), "K": types.Boolean(false)}
	stationeryPage(t, src, 1)["Group"] = group
	if err = pdfcpu.ComposePreparedStationery(t.Context(), dst, src); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err = api.WriteContext(t.Context(), dst, &buf); err != nil {
		t.Fatal(err)
	}
	out, err := api.ReadAndValidate(t.Context(), bytes.NewReader(buf.Bytes()), dst.Configuration)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(metadata, stationeryStream(t, out, out.RootDict["Metadata"]).Content) {
		t.Fatal("XMP changed")
	}
	actualFonts := stationeryFonts(t, out)
	for f := range fonts {
		if !actualFonts[f] {
			t.Fatal("original embedded font was lost")
		}
	}
	doc := stationeryDoc(t, out)
	kids, err := out.DereferenceArray(doc["K"])
	if err != nil {
		t.Fatal(err)
	}
	old := originalKids.(types.Array)
	if !reflect.DeepEqual(kids[:len(old)], old) {
		t.Fatal("body structure references changed")
	}
	section := stationeryDict(t, out, kids[len(old)])
	if section["Lang"] != types.StringLiteral("de-DE") {
		t.Fatal("source structure language lost")
	}
	sectionKids, err := out.DereferenceArray(section["K"])
	if err != nil {
		t.Fatal(err)
	}
	if len(sectionKids) != 6 {
		t.Fatalf("wanted 2 paragraphs on 3 pages, got %d", len(sectionKids))
	}
	forms := map[int]bool{}
	for n := 1; n <= out.PageCount; n++ {
		p := stationeryPage(t, out, n)
		if !reflect.DeepEqual(p["Annots"], annots[n-1]) {
			t.Fatalf("page %d links changed", n)
		}
		all, e := out.PageContent(p, n)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.HasSuffix(all, before[n-1]) {
			t.Fatalf("page %d body operators changed", n)
		}
		prefix := all[:len(all)-len(before[n-1])]
		if bytes.Contains(prefix, []byte("Artifact")) {
			t.Fatal("legal text was hidden as an artifact")
		}
		res := stationeryDict(t, out, p["Resources"])
		xo := stationeryDict(t, out, res["XObject"])
		formRef := xo["PreparedStationery"].(types.IndirectRef)
		forms[formRef.ObjectNumber.Value()] = true
		form := stationeryStream(t, out, formRef)
		if !bytes.Equal(form.Content, sourceContent) {
			t.Fatal("original stationery content changed")
		}
		if !reflect.DeepEqual(stationeryDict(t, out, form.Dict["Group"]), group) {
			t.Fatal("source transparency group lost")
		}
		if form.IntEntry("StructParents") == nil {
			t.Fatal("stationery disconnected from structure tree")
		}
	}
	for _, ref := range sectionKids {
		d := stationeryDict(t, out, ref)
		ks, e := out.DereferenceArray(d["K"])
		if e != nil {
			t.Fatal(e)
		}
		for _, k := range ks {
			mcr := stationeryDict(t, out, k)
			formRef := mcr["Stm"].(types.IndirectRef)
			if !forms[formRef.ObjectNumber.Value()] || mcr["MCID"] == nil {
				t.Fatal("missing text-to-structure connection")
			}
		}
	}
}

func TestPreparedStationeryRejectsUnsupportedInputs(t *testing.T) {
	tests := []struct {
		name, want string
		mutate     func(*testing.T, *model.Context, *model.Context)
	}{
		{"untagged", "tagged input", func(t *testing.T, d, s *model.Context) { delete(s.RootDict, "StructTreeRoot") }},
		{"annotation", "Annots", func(t *testing.T, d, s *model.Context) { stationeryPage(t, s, 1)["Annots"] = types.Array{} }},
		{"rotation", "unrotated", func(t *testing.T, d, s *model.Context) { stationeryPage(t, s, 1)["Rotate"] = types.Integer(90) }},
		{"crop", "crop box", func(t *testing.T, d, s *model.Context) {
			stationeryPage(t, s, 1)["CropBox"] = types.NewNumberArray(10, 10, 500, 800)
		}},
		{"origin", "zero-origin", func(t *testing.T, d, s *model.Context) {
			stationeryPage(t, s, 1)["MediaBox"] = types.NewNumberArray(10, 10, 605.2756, 851.8898)
		}},
		{"destination-geometry", "geometry", func(t *testing.T, d, s *model.Context) {
			stationeryPage(t, d, 2)["MediaBox"] = types.NewNumberArray(0, 0, 612, 792)
		}},
		{"destination-user-unit", "geometry", func(t *testing.T, d, s *model.Context) {
			stationeryPage(t, d, 2)["UserUnit"] = types.Float(2)
		}},
		{"javascript", "OpenAction", func(t *testing.T, d, s *model.Context) {
			s.RootDict["OpenAction"] = types.Dict{"S": types.Name("JavaScript"), "JS": types.StringLiteral("void(0)")}
		}},
		{"attachment", "AF", func(t *testing.T, d, s *model.Context) { s.RootDict["AF"] = types.Array{} }},
		{"layers", "OCProperties", func(t *testing.T, d, s *model.Context) { s.RootDict["OCProperties"] = types.Dict{} }},
		{"unsupported-role", "plain paragraphs", func(t *testing.T, d, s *model.Context) {
			a, _ := s.DereferenceArray(stationeryDoc(t, s)["K"])
			stationeryDict(t, s, a[0])["S"] = types.Name("H1")
		}},
		{"paragraph-attribute", "attribute", func(t *testing.T, d, s *model.Context) {
			a, _ := s.DereferenceArray(stationeryDoc(t, s)["K"])
			stationeryDict(t, s, a[0])["ActualText"] = types.StringLiteral("different")
		}},
		{"document-attribute", "document attribute", func(t *testing.T, d, s *model.Context) {
			stationeryDoc(t, s)["ActualText"] = types.StringLiteral("different")
		}},
		{"remapped-role", "remapped standard roles", func(t *testing.T, d, s *model.Context) {
			root := stationeryDict(t, s, s.RootDict["StructTreeRoot"])
			root["RoleMap"] = types.Dict{"P": types.Name("Span")}
		}},
		{"parent-tree-limits", "parent tree", func(t *testing.T, d, s *model.Context) {
			root := stationeryDict(t, d, d.RootDict["StructTreeRoot"])
			stationeryDict(t, d, root["ParentTree"])["Limits"] = types.Array{types.Integer(0), types.Integer(10)}
		}},
		{"duplicate-mcid", "duplicate MCID", func(t *testing.T, d, s *model.Context) {
			a, _ := s.DereferenceArray(stationeryDoc(t, s)["K"])
			stationeryDict(t, s, a[0])["K"] = types.Array{types.Integer(0), types.Integer(0)}
		}},
		{"missing-mcid", "contiguous", func(t *testing.T, d, s *model.Context) {
			a, _ := s.DereferenceArray(stationeryDoc(t, s)["K"])
			stationeryDict(t, s, a[0])["K"] = types.Array{types.Integer(1)}
		}},
		{"parent-tree", "parent tree", func(t *testing.T, d, s *model.Context) {
			root := stationeryDict(t, d, d.RootDict["StructTreeRoot"])
			root["ParentTree"] = types.Dict{"Kids": types.Array{}}
		}},
		{"cmyk-profile", "RGB output", func(t *testing.T, d, s *model.Context) {
			a, _ := s.DereferenceArray(s.RootDict["OutputIntents"])
			intent := stationeryDict(t, s, a[0])
			ref := intent["DestOutputProfile"].(types.IndirectRef)
			sd := s.Table[ref.ObjectNumber.Value()].Object.(types.StreamDict)
			sd.Dict["N"] = types.Integer(4)
		}},
		{"different-profile", "profiles differ", func(t *testing.T, d, s *model.Context) {
			a, _ := s.DereferenceArray(s.RootDict["OutputIntents"])
			intent := stationeryDict(t, s, a[0])
			sd, _ := s.NewStreamDictForBuf([]byte("different profile"))
			sd.Dict["N"] = types.Integer(3)
			sd.Encode()
			ref, _ := s.IndRefForNewObject(*sd)
			intent["DestOutputProfile"] = *ref
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, s := stationeryContext(t, "body"), stationeryContext(t, "letterhead")
			tt.mutate(t, d, s)
			err := pdfcpu.ComposePreparedStationery(t.Context(), d, s)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want refusal containing %q", err, tt.want)
			}
		})
	}
	t.Run("multipage", func(t *testing.T) {
		err := pdfcpu.ComposePreparedStationery(t.Context(), stationeryContext(t, "body"), stationeryContext(t, "body"))
		if err == nil || !strings.Contains(err.Error(), "one source page") {
			t.Fatal(err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		c, cancel := context.WithCancel(t.Context())
		cancel()
		if err := pdfcpu.ComposePreparedStationery(c, stationeryContext(t, "body"), stationeryContext(t, "letterhead")); err != context.Canceled {
			t.Fatal(err)
		}
	})
}
