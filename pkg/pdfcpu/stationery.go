/*
Copyright 2026 The pdfcpu Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
Unless required by law or agreed in writing, software is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND.
*/

package pdfcpu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/pdfcpu/pdfcpu/pkg/filter"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// ComposePreparedStationery is an experimental, deliberately restricted compositor.
// It copies a single prepared page under every destination page without converting
// its text or graphics. Both inputs must independently pass PDF/A-2a and PDF/UA-1
// validation before this call; this function is NOT a standards validator.
//
// The source structure must be Document -> P, with integer MCIDs and no nested
// structures, annotations, custom roles, or structure attributes. Page geometry
// and RGB output intent must match. Every occurrence is tagged (including repeats),
// in a new section after the original document's logical content. No unique legal
// text is hidden as an artifact. Full reading-order remediation is out of scope.
//
// The caller must validate the finished file independently and discard it on
// failure. On error either context may have been modified and must be discarded.
func ComposePreparedStationery(c context.Context, dst, src *model.Context) error {
	if dst == nil || src == nil || dst == src {
		return errors.New("stationery: distinct source and destination contexts required")
	}
	if err := c.Err(); err != nil {
		return err
	}
	if err := checkStationeryCatalog(src); err != nil {
		return err
	}
	if err := checkStationeryCatalog(dst); err != nil {
		return err
	}
	if err := src.EnsurePageCount(); err != nil {
		return err
	}
	if src.PageCount != 1 {
		return errors.New("stationery: exactly one source page required")
	}
	if err := dst.EnsurePageCount(); err != nil {
		return err
	}
	srcPage, srcRef, attrs, err := src.PageDict(c, 1, false)
	if err != nil {
		return err
	}
	if err = checkStationeryPage(srcPage); err != nil {
		return err
	}
	if attrs.Rotate != 0 || attrs.MediaBox == nil || attrs.MediaBox.LL.X != 0 || attrs.MediaBox.LL.Y != 0 {
		return errors.New("stationery: unrotated zero-origin source required")
	}
	if attrs.CropBox != nil && !sameStationeryBox(attrs.MediaBox, attrs.CropBox) {
		return errors.New("stationery: source crop box must equal media box")
	}
	sourceIntent, err := stationeryIntent(src)
	if err != nil {
		return err
	}
	destIntent, err := stationeryIntent(dst)
	if err != nil {
		return err
	}
	if !bytes.Equal(sourceIntent, destIntent) {
		return errors.New("stationery: source and destination output profiles differ")
	}

	sourceRoot, sourceDoc, _, err := stationeryDocument(src)
	if err != nil {
		return err
	}
	paragraphs, owners, err := stationeryParagraphs(src, sourceRoot, sourceDoc, *srcRef, srcPage)
	if err != nil {
		return err
	}
	destRoot, destDoc, destDocRef, err := stationeryDocument(dst)
	if err != nil {
		return err
	}
	parentTree, err := dst.DereferenceDict(destRoot["ParentTree"])
	if err != nil {
		return err
	}
	if parentTree == nil || parentTree["Kids"] != nil || parentTree["Limits"] != nil {
		return errors.New("stationery: flat destination parent tree required")
	}
	nums, err := dst.DereferenceArray(parentTree["Nums"])
	if err != nil {
		return err
	}
	if len(nums)%2 != 0 {
		return errors.New("stationery: invalid destination parent tree")
	}
	nextKey := 0
	for i := 0; i < len(nums); i += 2 {
		n, ok := nums[i].(types.Integer)
		if !ok {
			return errors.New("stationery: invalid parent tree key")
		}
		nextKey = max(nextKey, int(n)+1)
	}
	if n := destRoot.IntEntry("ParentTreeNextKey"); n != nil {
		nextKey = max(nextKey, *n)
	}

	// Check all destination geometry before copying anything.
	for p := 1; p <= dst.PageCount; p++ {
		page, _, a, err := dst.PageDict(c, p, false)
		if err != nil {
			return err
		}
		if page["UserUnit"] != nil || a.Rotate != 0 || !sameStationeryBox(attrs.MediaBox, a.MediaBox) || (a.CropBox != nil && !sameStationeryBox(a.MediaBox, a.CropBox)) {
			return fmt.Errorf("stationery: page %d geometry differs", p)
		}
	}
	content, err := src.PageContent(srcPage, 1)
	if err != nil {
		return err
	}
	resources := attrs.Resources.Clone()
	migrated := map[int]int{}
	resources, err = migrateObject(c, resources, src, dst, migrated)
	if err != nil {
		return err
	}
	var group types.Object
	if g := srcPage["Group"]; g != nil {
		group, err = migrateObject(c, g.Clone(), src, dst, migrated)
		if err != nil {
			return err
		}
	}
	section := types.Dict{"Type": types.Name("StructElem"), "S": types.Name("Sect"), "P": destDocRef}
	lang := sourceDoc["Lang"]
	if lang == nil {
		lang = src.RootDict["Lang"]
	}
	if lang != nil {
		v, err := src.Dereference(lang)
		if err != nil {
			return err
		}
		section["Lang"] = v.Clone()
	}
	sectionRef, err := dst.IndRefForNewObject(section)
	if err != nil {
		return err
	}
	sectionKids := types.Array{}
	for p := 1; p <= dst.PageCount; p++ {
		if err := c.Err(); err != nil {
			return err
		}
		page, pageRef, a, err := dst.PageDict(c, p, false)
		if err != nil {
			return err
		}
		key := nextKey
		nextKey++
		form := types.StreamDict{Dict: types.Dict{"Type": types.Name("XObject"), "Subtype": types.Name("Form"), "BBox": attrs.MediaBox.Array(), "Resources": resources, "StructParents": types.Integer(key)}, Content: content, FilterPipeline: []types.PDFFilter{{Name: filter.Flate}}}
		if group != nil {
			form.Dict["Group"] = group
		}
		form.InsertName("Filter", filter.Flate)
		if err = form.Encode(); err != nil {
			return err
		}
		formRef, err := dst.IndRefForNewObject(form)
		if err != nil {
			return err
		}
		refs := make([]types.IndirectRef, len(paragraphs))
		for i, para := range paragraphs {
			d := types.Dict{"Type": types.Name("StructElem"), "S": types.Name("P"), "P": *sectionRef, "Pg": *pageRef}
			kids := types.Array{}
			for _, mcid := range para {
				kids = append(kids, types.Dict{"Type": types.Name("MCR"), "MCID": types.Integer(mcid), "Pg": *pageRef, "Stm": *formRef})
			}
			d["K"] = kids
			ref, err := dst.IndRefForNewObject(d)
			if err != nil {
				return err
			}
			refs[i] = *ref
			sectionKids = append(sectionKids, *ref)
		}
		parents := make(types.Array, len(owners))
		for mcid, owner := range owners {
			parents[mcid] = refs[owner]
		}
		nums = append(nums, types.Integer(key), parents)
		// Clone inherited dictionaries so changing one page cannot alter its siblings.
		res := a.Resources.Clone().(types.Dict)
		xobjects := types.NewDict()
		if r := res["XObject"]; r != nil {
			d, err := dst.DereferenceDict(r)
			if err != nil {
				return err
			}
			xobjects = d.Clone().(types.Dict)
		}
		name := "PreparedStationery"
		for n := 1; xobjects[name] != nil; n++ {
			name = fmt.Sprintf("PreparedStationery%d", n)
		}
		xobjects[name] = *formRef
		res["XObject"] = xobjects
		page["Resources"] = res
		stream, err := dst.NewStreamDictForBuf([]byte("q /" + name + " Do Q\n"))
		if err != nil {
			return err
		}
		if err = stream.Encode(); err != nil {
			return err
		}
		streamRef, err := dst.IndRefForNewObject(*stream)
		if err != nil {
			return err
		}
		existing, err := dst.Dereference(page["Contents"])
		if err != nil {
			return err
		}
		contents := types.Array{*streamRef}
		if arr, ok := existing.(types.Array); ok {
			contents = append(contents, arr...)
		} else if page["Contents"] != nil {
			contents = append(contents, page["Contents"])
		}
		page["Contents"] = contents
	}
	section["K"] = sectionKids
	children, err := stationeryArray(dst, destDoc["K"])
	if err != nil {
		return err
	}
	destDoc["K"] = append(children, *sectionRef)
	parentTree["Nums"] = nums
	destRoot["ParentTreeNextKey"] = types.Integer(nextKey)
	return nil
}

func sameStationeryBox(a, b *types.Rectangle) bool {
	return a != nil && b != nil && math.Abs(a.LL.X-b.LL.X) < 0.001 && math.Abs(a.LL.Y-b.LL.Y) < 0.001 && math.Abs(a.UR.X-b.UR.X) < 0.001 && math.Abs(a.UR.Y-b.UR.Y) < 0.001
}

func checkStationeryCatalog(ctx *model.Context) error {
	for _, key := range []string{"AcroForm", "OpenAction", "AA", "OCProperties", "Collection", "AF", "Perms"} {
		if ctx.RootDict[key] != nil {
			return fmt.Errorf("stationery: catalog %s is unsupported", key)
		}
	}
	if ctx.Encrypt != nil {
		return errors.New("stationery: encrypted input is unsupported")
	}
	if names, err := ctx.DereferenceDict(ctx.RootDict["Names"]); err != nil {
		return err
	} else {
		for k := range names {
			if k != "Dests" {
				return fmt.Errorf("stationery: name tree %s is unsupported", k)
			}
		}
	}
	return nil
}

func checkStationeryPage(page types.Dict) error {
	for _, key := range []string{"Annots", "AA", "Trans", "Dur", "UserUnit"} {
		if page[key] != nil {
			return fmt.Errorf("stationery: source page %s is unsupported", key)
		}
	}
	return nil
}

func stationeryIntent(ctx *model.Context) ([]byte, error) {
	intents, err := ctx.DereferenceArray(ctx.RootDict["OutputIntents"])
	if err != nil {
		return nil, err
	}
	if len(intents) != 1 {
		return nil, errors.New("stationery: exactly one output intent required")
	}
	d, err := ctx.DereferenceDict(intents[0])
	if err != nil {
		return nil, err
	}
	sd, _, err := ctx.DereferenceStreamDict(d["DestOutputProfile"])
	if err != nil {
		return nil, err
	}
	if sd == nil || sd.IntEntry("N") == nil || *sd.IntEntry("N") != 3 {
		return nil, errors.New("stationery: RGB output profile required")
	}
	if err = sd.Decode(); err != nil {
		return nil, err
	}
	return sd.Content, nil
}

func stationeryArray(ctx *model.Context, o types.Object) (types.Array, error) {
	v, err := ctx.Dereference(o)
	if err != nil {
		return nil, err
	}
	if a, ok := v.(types.Array); ok {
		return a, nil
	}
	if v == nil {
		return nil, nil
	}
	return types.Array{o}, nil
}

func stationeryDocument(ctx *model.Context) (types.Dict, types.Dict, types.IndirectRef, error) {
	root, err := ctx.DereferenceDict(ctx.RootDict["StructTreeRoot"])
	if err != nil {
		return nil, nil, types.IndirectRef{}, err
	}
	if root == nil {
		return nil, nil, types.IndirectRef{}, errors.New("stationery: tagged input required")
	}
	kids, err := stationeryArray(ctx, root["K"])
	if err != nil {
		return nil, nil, types.IndirectRef{}, err
	}
	if len(kids) != 1 {
		return nil, nil, types.IndirectRef{}, errors.New("stationery: single Document structure required")
	}
	ref, ok := kids[0].(types.IndirectRef)
	if !ok {
		return nil, nil, ref, errors.New("stationery: indirect Document required")
	}
	doc, err := ctx.DereferenceDict(ref)
	if err != nil {
		return nil, nil, ref, err
	}
	if doc.NameEntry("S") == nil || *doc.NameEntry("S") != "Document" {
		return nil, nil, ref, errors.New("stationery: Document structure required")
	}
	return root, doc, ref, nil
}

func stationeryParagraphs(ctx *model.Context, root, doc types.Dict, pageRef types.IndirectRef, page types.Dict) ([][]int, []int, error) {
	for k := range doc {
		switch k {
		case "Type", "S", "P", "K", "Lang":
		default:
			return nil, nil, fmt.Errorf("stationery: document attribute %s unsupported", k)
		}
	}
	roles, err := ctx.DereferenceDict(root["RoleMap"])
	if err != nil {
		return nil, nil, err
	}
	if roles["P"] != nil || roles["Document"] != nil {
		return nil, nil, errors.New("stationery: remapped standard roles unsupported")
	}
	for _, k := range []string{"ClassMap", "IDTree", "Namespaces"} {
		if root[k] != nil {
			return nil, nil, fmt.Errorf("stationery: source %s unsupported", k)
		}
	}
	kids, err := stationeryArray(ctx, doc["K"])
	if err != nil {
		return nil, nil, err
	}
	if len(kids) == 0 {
		return nil, nil, errors.New("stationery: source has no paragraphs")
	}
	paras := make([][]int, len(kids))
	mapping := map[int]int{}
	maxID := -1
	for i, kid := range kids {
		d, err := ctx.DereferenceDict(kid)
		if err != nil {
			return nil, nil, err
		}
		for k := range d {
			switch k {
			case "Type", "S", "P", "Pg", "K":
			default:
				return nil, nil, fmt.Errorf("stationery: paragraph attribute %s unsupported", k)
			}
		}
		if d.NameEntry("S") == nil || *d.NameEntry("S") != "P" || d["Pg"] != pageRef {
			return nil, nil, errors.New("stationery: only page-local plain paragraphs supported")
		}
		ids, err := stationeryArray(ctx, d["K"])
		if err != nil {
			return nil, nil, err
		}
		if len(ids) == 0 {
			return nil, nil, errors.New("stationery: empty paragraph")
		}
		for _, id := range ids {
			n, ok := id.(types.Integer)
			if !ok || n < 0 || n > 10000 {
				return nil, nil, errors.New("stationery: bounded integer MCIDs required")
			}
			if _, exists := mapping[int(n)]; exists {
				return nil, nil, errors.New("stationery: duplicate MCID")
			}
			mapping[int(n)] = i
			maxID = max(maxID, int(n))
			paras[i] = append(paras[i], int(n))
		}
	}
	if maxID+1 != len(mapping) {
		return nil, nil, errors.New("stationery: contiguous MCIDs required")
	}
	owners := make([]int, len(mapping))
	for n, i := range mapping {
		owners[n] = i
	}
	pt, err := ctx.DereferenceDict(root["ParentTree"])
	if err != nil {
		return nil, nil, err
	}
	if pt == nil || pt["Kids"] != nil {
		return nil, nil, errors.New("stationery: flat source parent tree required")
	}
	nums, err := ctx.DereferenceArray(pt["Nums"])
	if err != nil {
		return nil, nil, err
	}
	sp := page.IntEntry("StructParents")
	if len(nums) != 2 || sp == nil || nums[0] != types.Integer(*sp) {
		return nil, nil, errors.New("stationery: source parent tree mismatch")
	}
	arr, err := ctx.DereferenceArray(nums[1])
	if err != nil {
		return nil, nil, err
	}
	if len(arr) != len(owners) {
		return nil, nil, errors.New("stationery: source MCID ownership mismatch")
	}
	for n, i := range owners {
		if arr[n] != kids[i] {
			return nil, nil, errors.New("stationery: source MCID owner mismatch")
		}
	}
	return paras, owners, nil
}
