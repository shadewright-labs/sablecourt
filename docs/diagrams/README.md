# Diagram standard

This page states how diagrams in the Sablecourt docs are drawn. It applies to every standalone SVG in this folder and to every Mermaid block in a page. A change that alters what talks to what, a set of states, a trust boundary, or what a harness can read, write or reach ships with the matching diagram change, including its description and alt text, in the same pull request.

## Mermaid or SVG

A diagram stays a Mermaid block when it is small and sits next to the text it explains:

- A sequence of up to 5 participants and about 10 messages.
- A state diagram of up to 8 states.
- A flowchart of up to 9 nodes with one or two decisions.
- Any diagram in an ADR or design page that changes often, because text diffs read better than drawings.

A Mermaid block carries no colours and no `init` directive, so GitHub themes it for the reader. A step that is designed and not built is marked `*(planned)*` in a sequence and with `classDef planned stroke-dasharray: 5 5` in a flowchart. Do not draw ASCII art.

A diagram becomes a standalone SVG when it is an overview a reader meets first, when it needs boundaries Mermaid cannot draw (nested trust zones, credential scopes, outside providers), when it needs refused paths in red or planned parts dashed, when Mermaid's layout crosses lines, or when more than one page uses it. Never draw the same flow twice on one page, once in each form.

## Files and links

SVG files live in this folder. Link each one from its page as an image inside a link to the file, with one sentence of alt text:

```markdown
[![Alt text in one sentence.](diagrams/name.svg)](diagrams/name.svg "Open full size")
```

From `docs/adr/` and from pages one folder down, adjust the relative path (`../diagrams/name.svg`). Every node and every meaningful line in a diagram is named in the text of the page that shows it.

## Pick the type by what the reader must see

| The reader must see | Type |
| --- | --- |
| which parts exist and what talks to what | components |
| a stack where each layer sits on the one below | layers |
| steps with a branch | flow |
| who sends what to whom, in order | sequence |
| the states of one thing and what moves it | states |
| who does which step | swimlane |
| what changed | before and after |
| what is inside what (trust boundaries, sandboxes, scopes) | nested |
| what happened when | timeline |
| who may do which action | matrix |

Say the point of the diagram in one sentence before drawing. If that sentence needs "and" twice, draw two diagrams. There is no fixed box limit, but above about 14 boxes ask whether it is two diagrams.

## SVG rules

- Dark only: one palette, no light variant. A standalone `.svg` with no HTML wrapper, script, link, external font, image or animation. Text stays `<text>`. The font stack is `system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif`.
- The root has `role="img"`, `aria-labelledby`, a `<title>` of 2 to 5 words and a `<desc>` that names every node and every line that carries meaning. Neither mentions an issue number.
- Lines run horizontally or vertically with at most one right-angle bend. No line crosses another line or a box.
- Trust boundaries are never dropped or moved to tidy a layout. Host, daemon, gates, harness container, internal network, proxy and providers sit in the zone they belong to.
- A line that carries a condition, a verb or a noun has a label of three words or fewer. A legend appears only when two or more box or line styles carry meaning.
- Refusals are words as well as colour: a red box or line also says "never" or "denied". Planned or parked parts are dashed.
- No em or en dashes in any text, and no issue or pull request numbers or dated decisions inside the picture.

## Colours

Every colour is a Shadewright Labs brand token. No other value is allowed, and no stroke that carries meaning uses opacity.

| Role | Value | Used for |
| --- | --- | --- |
| Canvas | `#08060e` | background rectangle (`rx` 18), the only full-size rectangle |
| Box fill | `#14111c` | every node surface |
| Lane or zone fill | `#111422` | a lane or trust zone that groups boxes |
| Lane stroke, lifelines | `#373040` | border of a plain lane and sequence lifelines, never a line that carries meaning |
| Heading text | `#efe9dc` | node names |
| Body text | `#bbb4c5` | detail lines, labels on lines, trust-boundary frame and caption |
| Box stroke, neutral line, planned | `#8a8594` | ordinary node border, reference lines, captions, dashed planned or parked boxes |
| Accent | `#2dd4bf` | the node the page is about (stroke, with a 22% to 6% glow fill), main-flow lines and arrowheads, step numbers |
| Outside or provider | `#7fd6b4` | border of a provider, Git host or system outside Sablecourt |
| Waiting for a person | `#f0b86a` | border of a step that waits for an approval or carries a budget warning |
| Refused | `#f87171` | blocked or denied boxes, refused lines and arrowheads, and the word that says "denied" or "never" |

Use the accent node at most twice per diagram; once is normal.

## Type and geometry

| Class | Size | Weight | Use |
| --- | --- | --- | --- |
| Node name | 15 | 650 | name inside a box |
| Detail | 12.5 | 400 | second and third line in a box, at most two lines |
| Line label | 12 | 400 | text on a line |
| Lane title | 13 | 600, upper case, letter spacing 0.14 em | lane or zone title |
| Caption | 12 | 600, letter spacing 0.06 em | trust-boundary or scope caption |
| Refusal | 13 | 600 | refusal or denial label |

No text is smaller than 12 in a new diagram. Canvas width is 1120, or 1280 for a wide flow and 1000 for a narrow one; height follows the content in multiples of 4 with 24 of margin. Boxes sit on a grid of 4: 210 by 64 for two text lines, 210 by 72 for three, corner radius 10 (12 for the accent node). Gaps are 48 or more between boxes in a row (72 when the line carries a label) and 40 or more between rows. A lane has radius 14 and 20 of padding. Strokes are 1.2 for a box, 1.6 for the accent node, 1.8 for a main-flow line and 1.4 dashed (`4 4`) for a reference, planned or refused line. A trust-boundary frame is dashed `5 5` in `#bbb4c5`, 1.2 wide.

Three arrowhead markers are shared: accent for main flow, grey for reference or planned lines, red for refused lines.

## Before you finish

1. Check each box against the page text: it sits in the zone the page names, and nothing appears that the page does not say.
2. Confirm no line crosses a line or a box, and every label is a word the page already introduced.
3. Render the SVG at about 800 px wide and read it for contrast and overlap.
4. Run the checker script for the mechanical rules above (colours, sizes, dashes, forbidden elements, title and description). It is kept in the maintainers' workspace and is not part of this repository.
