// Standalone experiment driver. It is not a production export command.
package main

import (
	"context"
	"fmt"
	"os"
	"regexp"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 4 {
		return fmt.Errorf("usage: probe roundtrip|transparent|missing-font|missing-unicode|artifact-source input.pdf output.pdf; probe compose|artifact-control body.pdf stationery.pdf output.pdf")
	}
	mode := os.Args[1]
	if mode == "compose" || mode == "artifact-control" || mode == "stock-watermark" {
		if len(os.Args) != 5 {
			return fmt.Errorf("composition requires body, stationery and output")
		}
	} else if len(os.Args) != 4 {
		return fmt.Errorf("unexpected arguments")
	}
	conf := model.NewDefaultConfiguration()
	conf.PreserveInfoDict = true
	conf.Optimize = false
	conf.OptimizeBeforeWriting = false
	read := func(path string) (*model.Context, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return api.ReadAndValidate(context.Background(), f, conf)
	}
	ctx, err := read(os.Args[2])
	if err != nil {
		return err
	}
	switch mode {
	case "compose":
		src, err := read(os.Args[3])
		if err != nil {
			return err
		}
		if err = pdfcpu.ComposePreparedStationery(context.Background(), ctx, src); err != nil {
			return err
		}
	case "artifact-control", "stock-watermark":
		// Deliberately wrong semantic control: visible legal text is treated as a watermark.
		wm, err := pdfcpu.ParsePDFWatermarkDetails(context.Background(), os.Args[3], "scale:1 abs, pos:bl, rot:0", false, types.POINTS, conf)
		if err != nil {
			return err
		}
		pages := types.IntSet{}
		for n := 1; n <= ctx.PageCount; n++ {
			pages[n] = true
		}
		if err = pdfcpu.AddWatermarks(context.Background(), ctx, pages, wm); err != nil {
			return err
		}
		if mode == "artifact-control" {
			// Repair the stock watermark's unrelated optional-content violations,
			// so this negative control isolates the semantic accessibility loss.
			oc, err := ctx.DereferenceDict(ctx.RootDict["OCProperties"])
			if err != nil {
				return err
			}
			d, err := ctx.DereferenceDict(oc["D"])
			if err != nil {
				return err
			}
			d["Name"] = types.StringLiteral("Semantic negative control")
			d["Order"] = oc["OCGs"]
			delete(d, "AS")
		}

	case "transparent":
		page, _, attrs, err := ctx.PageDict(context.Background(), 1, false)
		if err != nil {
			return err
		}
		page["Group"] = types.Dict{"S": types.Name("Transparency"), "CS": types.Name("DeviceRGB"), "I": types.Boolean(true), "K": types.Boolean(false)}
		res := attrs.Resources.Clone().(types.Dict)
		states := types.NewDict()
		if r := res["ExtGState"]; r != nil {
			d, e := ctx.DereferenceDict(r)
			if e != nil {
				return e
			}
			states = d.Clone().(types.Dict)
		}
		states["ProofAlpha"] = types.Dict{"Type": types.Name("ExtGState"), "ca": types.Float(.4), "CA": types.Float(.4), "BM": types.Name("Multiply")}
		res["ExtGState"] = states
		page["Resources"] = res
		content, err := ctx.PageContent(page, 1)
		if err != nil {
			return err
		}
		content = append(content, []byte("\n/Artifact BMC q 0.2 0.4 0.8 rg 460 750 70 20 re f /ProofAlpha gs 0.8 0.3 0.1 rg 490 740 50 40 re f Q EMC\n")...)
		if err = replacePageContent(ctx, page, content); err != nil {
			return err
		}
	case "artifact-source":
		// Only for our known synthetic fixture: remove marked-content wrappers while
		// retaining text operators. This is not a general PDF content-stream parser.
		page, _, _, err := ctx.PageDict(context.Background(), 1, false)
		if err != nil {
			return err
		}
		content, err := ctx.PageContent(page, 1)
		if err != nil {
			return err
		}
		re := regexp.MustCompile(`/Span\s*<<\s*/MCID\s+\d+\s*>>\s*BDC|/Artifact\s+BMC|EMC`)
		content = re.ReplaceAll(content, nil)
		if err = replacePageContent(ctx, page, content); err != nil {
			return err
		}
		delete(page, "StructParents")
		delete(ctx.RootDict, "StructTreeRoot")
		delete(ctx.RootDict, "MarkInfo")
		// This intentionally invalid intermediate is only used to generate the
		// all-artifact negative control, never accepted by ComposePreparedStationery.
	case "missing-font", "missing-unicode":
		for _, entry := range ctx.Table {
			if d, ok := entry.Object.(types.Dict); ok {
				if mode == "missing-font" {
					delete(d, "FontFile")
					delete(d, "FontFile2")
					delete(d, "FontFile3")
				} else {
					delete(d, "ToUnicode")
				}
			}
		}
	case "roundtrip":
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	outPath := os.Args[len(os.Args)-1]
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	if err = api.WriteContext(context.Background(), ctx, out); err != nil {
		out.Close()
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	fmt.Println("wrote", outPath)
	return nil
}
func replacePageContent(ctx *model.Context, page types.Dict, content []byte) error {
	sd, err := ctx.NewStreamDictForBuf(content)
	if err != nil {
		return err
	}
	if err = sd.Encode(); err != nil {
		return err
	}
	ref, err := ctx.IndRefForNewObject(*sd)
	if err != nil {
		return err
	}
	page["Contents"] = *ref
	return nil
}
