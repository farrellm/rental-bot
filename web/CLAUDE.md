# CLAUDE.md — the frontend

The frontend half of the root `CLAUDE.md`. These conventions come from the same
design doc and are load-bearing in the same way: changing one is a design
decision, not a refactor. The stamp, the dispatch register, and the state
vocabulary they draw from stay in the root file, because the API produces them.

## Conventions that must not erode

- **Frontend colours and faces come from `web/src/styles/tokens.css`**, never a
  literal hex value. Numbers use `.mono` so the tabular figures line up. The
  visual language is the county record card; `web/src/styles/card.css` explains
  it.
- **An entry is not a box.** Labels are pre-printed and values are typed onto
  the rule, so an input has no fill and no border except the line under it
  (`web/src/styles/controls.css`). Read and amend states share the same row
  geometry, which is what makes amending a card in place cost no reflow. Don't
  add a fill on focus — the focus ring is the indicator, and a fill draws back
  the box the whole design avoids.
- **Buttons are ink on stock**: a rule and a word, never a filled rectangle.
  Removing a row from a list is `.button--quiet` — a word in the margin with no
  rule, revealed on hover or focus and always legible where there is no hover.
  A bordered button on every line turns a list into a column of buttons with
  some content beside it.
- **A section of a property record is a divider tab**, cut from the same stock
  (`.stock` in `card.css`) and lapping the card's top rule by a pixel. Below
  40rem the tabs wrap, so the current one gets four edges instead — a joint
  drawn to a row that is not the card reads as a bug.
- **Severity is a margin mark, not a stamp.** The stamp means "the state of
  this thing"; a notice's severity is a property of it. On the dispatch
  register it is a `.stamped` word in the margin in the severity's own ink, the
  way a clerk marks priority beside an entry. Giving it a stamp would make the
  one bold mark on a card stop meaning one thing.
- **The lease term rule is the one drawn thing in the application** and it
  stays that way. It draws only what the dates say: a month-to-month lease gets
  no end tick, because inventing one asserts a fact the record does not hold.
  Dates are differenced at UTC midnight — they were stored without a timezone,
  and anything else makes "ends today" depend on where the reader is sitting.
- **Nouns carry the metaphor, verbs stay plain.** A "record" with a "file
  number" and an AMENDING stamp; buttons that say "Sign in", "Save changes",
  "Add unit".

## Laptop and iPhone, both

Treat 320px (iPhone SE) through 1920px as the supported range, portrait and
landscape. The card layout switches at `40rem`: two columns above, stacked
below.

What has to hold at every width:

- No horizontal scrolling, ever. `document.scrollWidth` must equal
  `clientWidth`.
- Nothing overlaps. Reserve space with layout — flex or grid siblings — rather
  than a fixed padding guessed against another element's width. That guess is
  what broke the ledger against the stamp at 660px.
- Text stays legible: no tap target or control below 44px, and no body text
  below 11px.
- No form control under 16px on a touch device. Safari auto-zooms the viewport
  when it focuses one and never restores the scale. `--size-body` goes to 1rem
  under `(pointer: coarse)` in `tokens.css`, which moves the read value with
  the entry so amending still costs no reflow.
- Layout survives inflated type. A flex item needs `min-width: 0` before
  `max-width` will constrain it, because the automatic minimum is the
  min-content width and a long word will not break on its own.

Verify with real device emulation, not a resized desktop viewport — a desktop
context narrowed to 320px applies text autosizing that a real iPhone does not,
and will report failures that do not exist. Wait for `networkidle` **and**
`document.fonts.ready` before measuring; the unstyled first paint is much
wider than the real layout and produces phantom overflow.
