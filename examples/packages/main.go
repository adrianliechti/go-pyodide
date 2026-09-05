// Command packages bundles a set of PyPI packages and runs them.
//
// It resolves the requirements below against PyPI, keeping only pure-Python
// wheels (the interpreter cannot load native extensions), downloads them,
// installs them into a persistent site directory and runs a script that
// exercises supported packages. Packages that need unavailable dependencies
// or standard-library modules are reported as skipped.
//
//	go run ./examples/packages            # wheels and site dir under examples/packages/wheels
//	go run ./examples/packages <dir>
//
// From examples/packages, run `go run . wheels`. Run the whole package:
// `go run main.go` excludes the resolver defined in pypi.go.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pyodide "github.com/adrianliechti/go-pyodide"
)

var requirements = []string{
	"seaborn",
	"et-xmlfile",
	"openpyxl",
	"xlsxwriter",
	"python-docx",
	"python-pptx",
	"docx2txt",
	"pypdf",
	// Pinned to the exact pdfminer.six pdfplumber depends on, so we bundle the
	// version it was tested against rather than whatever PyPI serves as latest.
	"pdfminer.six==20251230",
	"pdfplumber",
	"reportlab",
	"markdown",
	"markdownify",
	"tabulate",
	// Pin to last release before red-black-tree-mod was added — that dep only
	// ships as an sdist and our bundler only handles pure-Python wheels.
	"extract-msg==0.36.5",
}

func main() {
	ctx := context.Background()

	dir := "examples/packages/wheels"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}

	// 1. Resolve.
	res := NewResolver()
	for _, spec := range requirements {
		if err := res.Resolve(ctx, spec); err != nil {
			log.Fatal(err)
		}
	}

	names := make([]string, 0, len(res.Packages))
	for n := range res.Packages {
		names = append(names, n)
	}
	sort.Strings(names)

	unavailable := make(map[string][]string)
	fmt.Printf("%-24s %-14s %s\n", "package", "version", "status")
	for _, n := range names {
		p := res.Packages[n]
		missing := res.NeedsNative(n)
		status := "pure wheel"
		switch {
		case p.Missing:
			missing = []string{n}
			status = "NO PURE WHEEL (native extension or sdist only)"
		case len(missing) > 0:
			status = "pure wheel, but dependencies have no pure wheel: " + strings.Join(missing, ", ")
		}
		if len(missing) > 0 {
			unavailable[n] = missing
		}
		fmt.Printf("%-24s %-14s %s\n", n, p.Version, status)
	}

	// 2. Download.
	var wheels []string
	for _, n := range names {
		p := res.Packages[n]
		if p.Missing {
			continue
		}
		path := filepath.Join(dir, p.Wheel)
		if _, err := os.Stat(path); err != nil {
			fmt.Println("downloading", p.Wheel)
			if err := download(ctx, p.URL, path); err != nil {
				log.Fatal(err)
			}
		}
		wheels = append(wheels, path)
	}

	// 3. Install into a site directory that survives across runs; wheels
	// already present are skipped.
	cache, _ := os.UserCacheDir()
	rt, err := pyodide.New(ctx,
		pyodide.WithCacheDir(filepath.Join(cache, "go-pyodide")),
		pyodide.WithSiteDir(filepath.Join(dir, "site")),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)

	for _, w := range wheels {
		if err := rt.AddWheel(ctx, w); err != nil {
			log.Fatal(err)
		}
	}
	pkgs, err := rt.Packages()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%d distributions installed in %s\n", len(pkgs), rt.SiteDir())
	fmt.Println("Only pure-Python wheels are installed; some packages cannot run with the available dependencies and standard library.")
	fmt.Println()

	// 4. Use them.
	out, err := os.MkdirTemp("", "pyodide-packages-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(out)

	metadata, err := json.Marshal(unavailable)
	if err != nil {
		log.Fatal(err)
	}
	err = rt.Run(ctx, demoRunner+demo, pyodide.RunOptions{
		Args:   []string{string(metadata)},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Mounts: []pyodide.Mount{{Path: "/out", Dir: out}},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func download(ctx context.Context, url, path string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(path + ".part")
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(path+".part", path)
}

const demoRunner = `
import importlib
import json
import re
import sys
import traceback

unavailable = json.loads(sys.argv[1])
counts = {"ok": 0, "skip": 0, "fail": 0}

def step(name, *packages, stdlib=()):
    def run(fn):
        try:
            missing = sorted({
                dep
                for package in (packages or (name,))
                for dep in unavailable.get(re.sub(r"[-_.]+", "-", package).lower(), [])
            })
            reasons = []
            if missing:
                reasons.append("no pure-Python wheel for: " + ", ".join(missing))
            for module in stdlib:
                try:
                    importlib.import_module(module)
                except ModuleNotFoundError as e:
                    reasons.append(f"stdlib {module} unavailable in this interpreter ({e})")
            if reasons:
                counts["skip"] += 1
                print(f"SKIP  {name}: {'; '.join(reasons)}")
                return fn
            result = fn()
            counts["ok"] += 1
            print(f"ok    {name}: {result}")
        except Exception as e:
            counts["fail"] += 1
            print(f"FAIL  {name}: {type(e).__name__}: {e}")
            traceback.print_exc()
        return fn
    return run

def finish():
    print(f"\n{counts['ok']} passed, {counts['skip']} skipped (unsupported), {counts['fail']} failed")
    if counts["fail"]:
        raise SystemExit(1)
`

const demo = `
@step("xlsxwriter + openpyxl", "xlsxwriter", "openpyxl")
def _():
    import xlsxwriter, openpyxl
    with xlsxwriter.Workbook("/out/report.xlsx") as wb:
        ws = wb.add_worksheet("Sales")
        ws.write_row(0, 0, ["region", "revenue"])
        for i, (r, v) in enumerate([("north", 120), ("south", 80), ("west", 95)], 1):
            ws.write_row(i, 0, [r, v])
        ws.write_formula(4, 1, "=SUM(B2:B4)")
    ws = openpyxl.load_workbook("/out/report.xlsx")["Sales"]
    return [[c.value for c in row] for row in ws.iter_rows()]

@step("pypdf")
def _():
    from pypdf import PdfReader, PdfWriter
    # reportlab needs Pillow (native), so write a minimal PDF by hand.
    content = b"BT /F1 24 Tf 72 720 Td (Hello from pypdf inside Go) Tj ET"
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
        b"<< /Length %d >>stream\n" % len(content) + content + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    pdf, offsets = b"%PDF-1.4\n", []
    for i, o in enumerate(objs, 1):
        offsets.append(len(pdf))
        pdf += b"%d 0 obj\n" % i + o + b"\nendobj\n"
    xref = len(pdf)
    pdf += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
    pdf += b"".join(b"%010d 00000 n \n" % off for off in offsets)
    pdf += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, xref)
    open("/out/hello.pdf", "wb").write(pdf)

    text = PdfReader("/out/hello.pdf").pages[0].extract_text()
    writer = PdfWriter()
    writer.append("/out/hello.pdf")
    writer.pages[0].rotate(90)
    writer.add_metadata({"/Title": "rotated"})
    writer.write("/out/rotated.pdf")  # streams are FlateDecode: zlib at work
    r = PdfReader("/out/rotated.pdf")
    return text, r.metadata.title, r.pages[0].rotation

@step("pdfminer.six")
def _():
    from pdfminer.high_level import extract_text
    return extract_text("/out/hello.pdf").strip()

@step("docx2txt")
def _():
    import zipfile, docx2txt
    # a minimal .docx built by hand: python-docx needs lxml, which is native
    body = "<w:p><w:r><w:t>Hello from a docx</w:t></w:r></w:p>"
    doc = f'<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>{body}</w:body></w:document>'
    with zipfile.ZipFile("/out/hello.docx", "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("[Content_Types].xml", '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>')
        z.writestr("word/document.xml", doc)
    return docx2txt.process("/out/hello.docx").strip()

@step("markdown + markdownify + tabulate", "markdown", "markdownify", "tabulate")
def _():
    import markdown, markdownify, tabulate
    html = markdown.markdown("# Title\n\nSome *emphasis* and a [link](https://example.com).")
    back = markdownify.markdownify(html).strip()
    table = tabulate.tabulate([["a", 1], ["b", 2]], headers=["key", "value"], tablefmt="github")
    return {"html": html, "markdown": back, "table": table.splitlines()}

@step("extract_msg", "extract-msg", stdlib=("ssl",))
def _():
    import extract_msg
    return extract_msg.__version__

for name, package in [("seaborn", "seaborn"), ("pdfplumber", "pdfplumber"),
                      ("docx", "python-docx"), ("pptx", "python-pptx"),
                      ("reportlab", "reportlab")]:
    @step(name, package)
    def _(name=name):
        __import__(name)
        return "imported"

finish()
`
