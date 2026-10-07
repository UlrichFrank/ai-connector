"""Markdown to PDF for the files sandbox: markdown -> HTML (python-markdown) -> PDF (WeasyPrint).

Usage: markdown_pdf.py <doc.json> with {"markdown": ..., "title": ..., "out": "/out/name.pdf"}.
Also importable: to_pdf(markdown_text, out_path, title="").
"""

import html
import json
import sys

import markdown
from weasyprint import CSS, HTML, default_url_fetcher

STYLE = """
@page {
  size: A4;
  margin: 22mm 20mm 22mm 20mm;
  @bottom-center { content: counter(page) " / " counter(pages);
                   font-family: "Noto Sans", "DejaVu Sans", sans-serif; font-size: 8.5pt; color: #777; }
}
html { font-family: "Noto Sans", "DejaVu Sans", sans-serif; font-size: 10.5pt;
       line-height: 1.5; color: #1d1d1f; }
body { margin: 0; }
h1, h2, h3, h4 { font-family: "Noto Sans", "DejaVu Sans", sans-serif; line-height: 1.25; color: #111;
                 page-break-after: avoid; break-after: avoid; }
h1 { font-size: 21pt; margin: 0 0 10pt; }
h1.doc-title { font-size: 24pt; border-bottom: 2px solid #2c5d8f; padding-bottom: 6pt; margin-bottom: 16pt; }
h2 { font-size: 15pt; margin: 18pt 0 6pt; border-bottom: 1px solid #ddd; padding-bottom: 3pt; }
h3 { font-size: 12.5pt; margin: 14pt 0 4pt; }
h4 { font-size: 11pt; margin: 12pt 0 4pt; }
p { margin: 0 0 7pt; orphans: 3; widows: 3; }
a { color: #2c5d8f; text-decoration: none; }
ul, ol { margin: 0 0 8pt; padding-left: 18pt; }
li { margin: 2pt 0; }
blockquote { margin: 8pt 0; padding: 4pt 12pt; border-left: 3px solid #c8d3df; color: #444; background: #f6f8fa; }
code { font-family: "Noto Sans Mono", "DejaVu Sans Mono", monospace; font-size: 9pt; background: #f2f3f5;
       padding: 0.5pt 3pt; border-radius: 3pt; }
pre { font-family: "Noto Sans Mono", "DejaVu Sans Mono", monospace; font-size: 8.8pt; line-height: 1.4;
      background: #f6f8fa; border: 1px solid #e1e4e8; border-radius: 4pt; padding: 7pt 9pt;
      white-space: pre-wrap; word-wrap: break-word; page-break-inside: avoid; }
pre code { background: none; padding: 0; font-size: inherit; }
.codehilite { background: #f6f8fa; border-radius: 4pt; margin: 0 0 8pt; }
.codehilite pre { margin: 0; }
table { border-collapse: collapse; width: 100%; margin: 6pt 0 10pt; font-size: 9.5pt; page-break-inside: auto; }
thead { display: table-header-group; }
tr { page-break-inside: avoid; }
th { background: #2c5d8f; color: #fff; font-weight: 600; text-align: left; padding: 4pt 6pt; }
td { padding: 4pt 6pt; border-bottom: 1px solid #e1e4e8; vertical-align: top; }
tbody tr:nth-child(even) td { background: #f6f8fa; }
hr { border: none; border-top: 1px solid #ddd; margin: 12pt 0; }
img { max-width: 100%; }
.footnote { font-size: 8.5pt; color: #555; }
"""


def offline_fetcher(url, *args, **kwargs):
    # only inline data: URLs; no network (there is none anyway) and no local files
    if url.startswith("data:"):
        return default_url_fetcher(url, *args, **kwargs)
    raise ValueError("external resources are not available in the sandbox: " + url[:80])


def to_pdf(text, out, title=""):
    body = markdown.markdown(
        text,
        extensions=["extra", "sane_lists", "codehilite", "toc", "smarty"],
        extension_configs={"codehilite": {"noclasses": True, "guess_lang": False, "pygments_style": "friendly"}},
        output_format="html5",
    )
    head_title = html.escape(title) if title else "Document"
    heading = '<h1 class="doc-title">%s</h1>' % html.escape(title) if title else ""
    page = (
        '<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head><body>%s%s</body></html>'
        % (head_title, heading, body)
    )
    HTML(string=page, url_fetcher=offline_fetcher).write_pdf(out, stylesheets=[CSS(string=STYLE)])


def main():
    with open(sys.argv[1], encoding="utf-8") as f:
        doc = json.load(f)
    to_pdf(doc.get("markdown", ""), doc["out"], doc.get("title", ""))


if __name__ == "__main__":
    main()
