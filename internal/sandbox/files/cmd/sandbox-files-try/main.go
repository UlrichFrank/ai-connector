// Command sandbox-files-try demonstrates the files sandbox: it builds the image if needed and makes a one-page PDF
// report with a chart, as the model would (make_file), then a PDF from Markdown (markdown_to_pdf).
//
//	go run ./internal/sandbox/files/cmd/sandbox-files-try [-out dir] [-build-only]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/philipparndt/ai-connector/internal/sandbox/files"
)

const reportCode = `
import os
import matplotlib.pyplot as plt
from reportlab.lib import colors
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import getSampleStyleSheet, ParagraphStyle
from reportlab.lib.units import cm
from reportlab.pdfbase import pdfmetrics
from reportlab.pdfbase.ttfonts import TTFont
from reportlab.platypus import SimpleDocTemplate, Paragraph, Spacer, Image, Table, TableStyle

pdfmetrics.registerFont(TTFont("DejaVu", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"))
pdfmetrics.registerFont(TTFont("DejaVu-Bold", "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"))

months = ["Jan", "Feb", "Mär", "Apr", "Mai", "Jun"]
kwh = [412, 365, 298, 221, 174, 150]
pv = [38, 72, 148, 231, 289, 312]

fig, ax = plt.subplots(figsize=(7, 3.2), dpi=150)
ax.bar(months, kwh, color="#2c5d8f", label="Verbrauch")
ax.plot(months, pv, color="#e07b39", marker="o", linewidth=2, label="PV-Ertrag")
ax.set_ylabel("kWh")
ax.spines[["top", "right"]].set_visible(False)
ax.legend(frameon=False)
fig.tight_layout()
fig.savefig("/tmp/chart.png")

styles = getSampleStyleSheet()
h = ParagraphStyle("h", parent=styles["Title"], fontName="DejaVu-Bold", fontSize=20)
p = ParagraphStyle("p", parent=styles["BodyText"], fontName="DejaVu", fontSize=10.5, leading=15)
doc = SimpleDocTemplate(os.environ["OUT_FILE"], pagesize=A4, leftMargin=2*cm, rightMargin=2*cm,
                        topMargin=2*cm, bottomMargin=2*cm, title="Energiebericht Halbjahr 1")
table = Table([["Monat", "Verbrauch (kWh)", "PV (kWh)", "Autarkie"]] +
              [[m, k, v, f"{min(v / k, 1):.0%}"] for m, k, v in zip(months, kwh, pv)],
              colWidths=[3*cm, 4*cm, 3.5*cm, 3*cm])
table.setStyle(TableStyle([
    ("FONT", (0, 0), (-1, -1), "DejaVu", 9.5),
    ("FONT", (0, 0), (-1, 0), "DejaVu-Bold", 9.5),
    ("BACKGROUND", (0, 0), (-1, 0), colors.HexColor("#2c5d8f")),
    ("TEXTCOLOR", (0, 0), (-1, 0), colors.white),
    ("ROWBACKGROUNDS", (0, 1), (-1, -1), [colors.white, colors.HexColor("#f2f5f8")]),
    ("ALIGN", (1, 0), (-1, -1), "RIGHT"),
]))
doc.build([
    Paragraph("Energiebericht – 1. Halbjahr", h),
    Paragraph("Der Verbrauch ist von Januar bis Juni um 64 % gesunken, während der PV-Ertrag stetig "
              "gestiegen ist. Im Juni deckte die Anlage den Bedarf vollständig (Größenordnung: "
              "312 kWh Ertrag gegenüber 150 kWh Verbrauch).", p),
    Spacer(1, 0.4*cm),
    Image("/tmp/chart.png", width=17*cm, height=17*cm*3.2/7),
    Spacer(1, 0.4*cm),
    table,
])
print("ok")
`

const markdownDoc = "## Zusammenfassung\n\nDie Übersicht über **Q2** – mit Umlauten (ä ö ü ß) und Emoji 🎉.\n\n" +
	"| Posten | Betrag |\n|---|---:|\n| Strom | 123,45 € |\n| Wasser | 67,00 € |\n\n" +
	"```python\ndef total(xs):\n    return sum(xs)\n```\n\n> Hinweis: erzeugt ohne Netzwerk.\n"

func main() {
	out := flag.String("out", "sandbox-files-out", "output dir")
	buildOnly := flag.Bool("build-only", false, "only build the image (if missing)")
	flag.Parse()
	s, err := files.New(files.Options{OutDir: *out, BuildLog: os.Stderr})
	if err != nil {
		fail(err)
	}
	ctx := context.Background()
	fmt.Println("image:", s.Image())
	t0 := time.Now()
	if err := s.EnsureImage(ctx); err != nil {
		fail(err)
	}
	fmt.Printf("image ready (%.1fs)\n", time.Since(t0).Seconds())
	if *buildOnly {
		return
	}

	for _, c := range []struct {
		tool string
		args map[string]string
	}{
		{"make_file", map[string]string{"filename": "Energiebericht.pdf", "code": reportCode, "language": "python"}},
		{"markdown_to_pdf", map[string]string{"filename": "zusammenfassung.pdf", "markdown": markdownDoc, "title": "Quartal 2"}},
	} {
		args, _ := json.Marshal(c.args)
		t0 := time.Now()
		r, err := s.Call(ctx, c.tool, args)
		if err != nil {
			fail(err)
		}
		fmt.Printf("\n== %s (%.1fs, error=%v)\n%s\nfiles: %v\n", c.tool, time.Since(t0).Seconds(), r.IsError, r.Text, r.Files)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
