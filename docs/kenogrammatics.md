# Kenogrammatics: lineage and limits

Status: conceptual note. The requirements remain the binding engineering
contracts.

Kenogram takes its name from the kenogrammatic lineage begun by Gotthard
Günther and developed by Rudolf Kaehr and Thomas Mahler. In this work,
kenograms are not signs with stable identity or meaning. They are the marks
through which morphograms become perceptible; relations among monomorphies and
loci matter, not the particular signs used to write them. Different
inscriptions can therefore exhibit the same pattern.

## Sources

These references serve different readers and should not be collapsed into one
claim of authority:

1. Gotthard Günther, [*Natural Numbers in Trans-Classic
   Systems*](https://www.vordenker.de/ggphilosophy/gg_natural-numbers.pdf)
   (1971), is the foundational primary source for the kenogrammatic structures
   and proto/deutero/trito distinctions from which the later work develops.
2. Thomas Mahler and Rudolf Kaehr, [*Morphogrammatik: Eine Einführung in die
   Theorie der logischen
   Form*](https://www.vordenker.de/rk/tm-rk_Morphogrammatik_Buch_1993.pdf)
   (1993, German), is the systematic book-length treatment.
3. Rudolf Kaehr, [*Morphogrammatics for
   Dummies*](https://www.vordenker.de/rk/rk_Morphogrammatics-for-Dummies_2010.pdf)
   (2010), is the approachable project-facing introduction.

The repository adapts one engineering intuition from this lineage. It does not
claim that the sources endorse Kenogram's security model or that the program
implements their formal systems.

The phrase "marked empty place" can be suggestive, but it is not a sufficient
definition. It risks turning a kenogram into one privileged symbol for absence.
The technical idea is plural and relational: identity is suspended so that a
pattern of sameness and difference across places can appear.

## The engineering adaptation

Kenogram is a Linux world provisioner, not a morphogrammatic calculus. Its name
commits the project to a methodological analogy rather than a one-to-one formal
translation:

- A declaration specifies an observable world-pattern.
- A generation is one material inscription of that pattern.
- Replacement may change the inscription while preserving the required
  observations.
- Runtime invariants judge realizations by behavior, not by an implementation's
  internal identity. In this limited engineering sense, conforming mechanisms
  are behaviorally equivalent.
- The provisioner contributes no request or policy authority from inside the
  world. It faithfully materializes the host-authored declaration.

This posture is most concrete in the network contract. A mechanism is acceptable
only if the same absence, visibility, reachability, and failure observations
hold at the real runtime boundary. The invariant set is deliberately more
normative than the mechanism used to satisfy it.

## Provenance is not ontology

Kenogram deliberately computes exact hashes. The declaration digest proves
which input bytes were read; the plan digest fingerprints the resolved semantic
plan; workspace and history digests carry evidence across replacement. Exact
plan-fingerprint equality is also used for safe operational adoption.

Those comparisons establish provenance and conservative operational sameness.
They do not claim that hashes define what a world ultimately *is*. Two mechanisms
or generations may differ in bytes and structure while satisfying the same
observable contract. Conversely, a matching label without matching evidence is
not enough to adopt runtime state.

## Where the analogy stops

Kenogram retains stable names, sequential generations, cryptographic hashes,
host-authored declarations, and ordinary Boolean validation. It defines no
morphogrammatic operators, retrograde continuations, or formal morphic
bisimulation. Terms such as *world-pattern*, *inscription*, and *behavioral
equivalence* in this repository are disciplined analogies, not claims of formal
equivalence with Kaehr's system.

The principle that absence precedes denial is Kenogram's own security posture.
It means an undeclared capability should not be present and then refused; it
should be missing from the world's observable structure. That principle is
compatible with the project's name, but it is not presented as a theorem or
teaching of kenogrammatics.

## Visual mark

The mark avoids a single privileged glyph or black core. It presents two
different fields of marks with the same grouping across five loci. Surface
identity changes while the relational decomposition remains. The open field is
not a symbol for nothingness; it gives the plural marks room to disclose their
pattern.
