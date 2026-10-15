#!/usr/bin/env python3
"""Independent regression evidence. Requires pinned Typst/veraPDF, qpdf and Poppler.
The compositor is deliberately NOT a validator. This runner validates both inputs
and final files, and checks real text/structure in addition to conformance reports.
"""
import base64
import collections
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2]
LAB = Path(__file__).resolve().parent
OUT = LAB / 'out'
FIX = LAB / 'fixtures'
TYPST = os.environ.get('TYPST', 'typst')
VERAPDF = os.environ.get('VERAPDF', 'verapdf')


def run(*args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, capture_output=True, **kwargs).stdout


def pdf_objects(path):
    return json.loads(run('qpdf', '--json', '--json-stream-data=inline', path))['qpdf'][1]


def obj(objects, value):
    if isinstance(value, str) and re.fullmatch(r'\d+ \d+ R', value):
        record = objects['obj:' + value]
        return record.get('value', record.get('stream'))
    return value


def catalog(objects):
    return obj(objects, objects['trailer']['value']['/Root'])


def content(objects, ref):
    return base64.b64decode(obj(objects, ref)['data'])


def font_hashes(objects):
    hashes = set()
    for record in objects.values():
        value = record.get('value', {})
        if not isinstance(value, dict):
            continue
        for key in ('/FontFile', '/FontFile2', '/FontFile3'):
            if key in value:
                hashes.add(hashlib.sha256(content(objects, value[key])).hexdigest())
    return hashes


def words(path):
    root = ET.fromstring(run('pdftotext', '-bbox', path, '-'))
    ns = {'x': 'http://www.w3.org/1999/xhtml'}
    return [collections.Counter((word.text, *(round(float(word.attrib[k]), 3) for k in
            ('xMin', 'yMin', 'xMax', 'yMax'))) for word in page.findall('x:word', ns))
            for page in root.findall('.//x:page', ns)]


def check_text(body, source, output):
    before, stationery, after = words(body), words(source), words(output)
    assert len(before) == len(after) == 3
    for page, (b, a) in enumerate(zip(before, after), 1):
        assert not b - a, f'body text/positions lost on page {page}: {b-a}'
        assert not stationery[0] - a, f'legal text/positions lost on page {page}: {stationery[0]-a}'
    text = run('pdftotext', '-layout', output, '-').decode()
    for phrase in ['Muster Versicherung GmbH', 'Musterstraße 12', 'Élodie Weiß',
                   'Jürgen Müller', 'HRB 123456', 'DE123456789', 'kontakt@test.finalwording.com']:
        assert text.count(phrase) == 3, f'missing legal detail: {phrase}'


def check_structure(body, source, output):
    b, s, o = (pdf_objects(p) for p in (body, source, output))
    root = obj(o, catalog(o)['/StructTreeRoot'])
    document = obj(o, root['/K'][0])
    original_root = obj(b, catalog(b)['/StructTreeRoot'])
    original_document = obj(b, original_root['/K'][0])
    assert document['/K'][:-1] == original_document['/K'], 'body structure changed'
    section = obj(o, document['/K'][-1])
    assert section.get('/S') == '/Sect', 'no accessible stationery section'
    assert len(section['/K']) == 6, 'legal paragraphs missing'
    form_refs = set()
    mcids = collections.defaultdict(set)
    for para_ref in section['/K']:
        para = obj(o, para_ref)
        assert para['/S'] == '/P'
        for mcr in para['/K']:
            assert mcr['/Type'] == '/MCR'
            form_ref = mcr['/Stm']
            form = obj(o, form_ref)
            assert '/StructParents' in form['dict'], 'unconnected stationery text'
            form_refs.add(form_ref)
            assert mcr['/MCID'] not in mcids[form_ref], 'duplicate content reference'
            mcids[form_ref].add(mcr['/MCID'])
    assert len(form_refs) == 3
    for ids in mcids.values():
        assert ids == {0, 1, 2, 3}, 'a legal text run is missing from reading order'
    source_page = next(v['value'] for v in s.values() if isinstance(v.get('value'), dict)
                       and v['value'].get('/Type') == '/Page')
    original_content = content(s, source_page['/Contents'])
    for ref in form_refs:
        assert content(o, ref) == original_content, 'stationery drawing/text operators changed'
        if '/Group' in source_page:
            assert obj(o, obj(o, ref)['dict']['/Group']) == obj(s, source_page['/Group'])
    assert font_hashes(s) <= font_hashes(o), 'stationery font data changed'
    assert font_hashes(b) <= font_hashes(o), 'body font data changed'
    assert content(b, catalog(b)['/Metadata']) == content(o, catalog(o)['/Metadata']), 'XMP changed'
    # Original page streams, tag objects, link annotations and destinations keep
    # their object numbers and exact values in this deliberately non-optimizing path.
    for key, record in b.items():
        value = record.get('value', {})
        if isinstance(value, dict) and (value.get('/Type') == '/Annot' or value.get('/S') == '/GoTo'):
            assert o[key] == record, f'link changed: {key}'
    return {'stationery_paragraphs': 6, 'tagged_text_runs': 12, 'original_font_programs_preserved': True}


def validate(path, expected):
    proc = subprocess.run([VERAPDF, '--format', 'json', str(path)], capture_output=True)
    report = json.loads(proc.stdout)
    (OUT / (path.stem + '-validation.json')).write_bytes(proc.stdout)
    jobs = report['report']['jobs']
    assert len(jobs) == 1
    results = jobs[0].get('validationResult', [])
    assert len(results) == 2, f'missing dual-profile validation for {path.name}: {report}'
    summaries = []
    for result in results:
        assert result.get('jobEndStatus') == 'normal', result
        assert result['compliant'] == expected, (path.name, result)
        summaries.append({'profile': result['profileName'], 'compliant': result['compliant'],
                          'passed_checks': result['details']['passedChecks'],
                          'failed_checks': result['details']['failedChecks']})
    return summaries


def pixels(path, page):
    return run('pdftoppm', '-r', '100', '-f', str(page), '-l', str(page), '-singlefile', path)


def main():
    OUT.mkdir(exist_ok=True)
    versions = {'typst': run(TYPST, '--version').decode().strip(),
                'verapdf': run(VERAPDF, '--version').decode().strip(),
                'qpdf': run('qpdf', '--version').decode().splitlines()[0],
                'go': run('go', 'version').decode().strip()}
    assert versions['typst'].startswith('typst 0.15.0 '), versions
    assert 'veraPDF 1.30.3' in versions['verapdf'], versions
    for name in ('body', 'letterhead'):
        run(TYPST, 'compile', '--creation-timestamp', '0', '--pdf-standard', 'a-2a,ua-1',
            FIX / (name + '.typ'), FIX / (name + '.pdf'))
    run('go', 'build', '-o', OUT / 'probe', './experiments/stationery/probe', cwd=ROOT)
    probe = OUT / 'probe'
    body, source = FIX / 'body.pdf', FIX / 'letterhead.pdf'
    result = {'versions': versions, 'validation': {}, 'fidelity': {}, 'negative_controls': {},
              'screen_reader': 'NOT PERFORMED: structural inspection is not a screen-reader audit',
              'reading_order': 'Body first, followed by six stationery paragraphs (all three occurrences). Repetition/order refinement remains open.'}
    for path in (body, source):
        result['validation'][path.stem] = validate(path, True)
    run(probe, 'roundtrip', body, OUT / 'roundtrip.pdf')
    result['validation']['roundtrip'] = validate(OUT / 'roundtrip.pdf', True)
    assert words(body) == words(OUT / 'roundtrip.pdf')
    for page in range(1, 4):
        assert pixels(body, page) == pixels(OUT / 'roundtrip.pdf', page), 'roundtrip pixels changed'
    result['fidelity']['roundtrip'] = 'all three pages pixel-identical; text positions unchanged'
    run(probe, 'transparent', source, OUT / 'transparent.pdf')
    result['validation']['transparent'] = validate(OUT / 'transparent.pdf', True)
    for label, stationery in [('composed', source), ('composed-transparent', OUT / 'transparent.pdf')]:
        output = OUT / (label + '.pdf')
        run(probe, 'compose', body, stationery, output)
        result['validation'][label] = validate(output, True)
        check_text(body, stationery, output)
        result['fidelity'][label] = check_structure(body, stationery, output)
        reference = OUT / (label + '-qpdf-reference.pdf')
        run('qpdf', body, '--underlay', stationery, '--repeat=1', '--', reference)
        for page in range(1, 4):
            assert pixels(output, page) == pixels(reference, page), f'{label}: changed pixels on page {page}'
        result['fidelity'][label]['pixels'] = 'all three pages identical to independent qpdf composition at 100 dpi'
    # Standards pass while the important legal information is hidden from AT.
    run(probe, 'artifact-source', source, OUT / 'artifact-source.pdf')
    run(probe, 'stock-watermark', body, OUT / 'artifact-source.pdf', OUT / 'stock-watermark.pdf')
    result['validation']['stock-watermark'] = validate(OUT / 'stock-watermark.pdf', False)
    run(probe, 'artifact-control', body, OUT / 'artifact-source.pdf', OUT / 'artifact-control.pdf')
    result['validation']['artifact-control'] = validate(OUT / 'artifact-control.pdf', True)
    check_text(body, source, OUT / 'artifact-control.pdf')
    artifact_objects = pdf_objects(OUT / 'artifact-control.pdf')
    # The text still extracts, but no structure node references the imported form.
    assert not any('/Stm' in json.dumps(v.get('value', {})) for v in artifact_objects.values())
    assert any(b'/Artifact' in base64.b64decode(v['stream']['data'])
               for v in artifact_objects.values() if 'stream' in v)
    try:
        check_structure(body, source, OUT / 'artifact-control.pdf')
    except AssertionError as error:
        result['negative_controls']['artifact-control'] = 'correctly rejected by semantic gate: ' + str(error)
    else:
        raise AssertionError('semantic gate accepted artifacted legal text')
    try:
        check_text(body, source, OUT / 'roundtrip.pdf')
    except AssertionError:
        result['negative_controls']['missing-stationery'] = 'correctly rejected despite both profiles passing'
    else:
        raise AssertionError('text gate accepted missing legal details')
    for mode in ('missing-font', 'missing-unicode'):
        bad = OUT / (mode + '.pdf')
        run(probe, mode, OUT / 'composed.pdf', bad)
        result['validation'][mode] = validate(bad, False)
        result['negative_controls'][mode] = 'correctly rejected by both veraPDF profiles'
    test_output = run('go', 'test', './pkg/pdfcpu', '-run', 'TestPreparedStationery', '-count=1', '-v', cwd=ROOT)
    (OUT / 'unit-tests.txt').write_bytes(test_output)
    result['unit_tests'] = test_output.decode()
    (OUT / 'summary.json').write_text(json.dumps(result, indent=2, ensure_ascii=False) + '\n')
    print(json.dumps({k:v for k,v in result.items() if k not in ('unit_tests','versions')}, indent=2, ensure_ascii=False))

if __name__ == '__main__':
    main()
