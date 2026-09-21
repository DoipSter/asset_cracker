# Errata to the v3 measurement protocol

Append-only. The rule is section 0.1 of `docs/v3-measurement-protocol.md`: an erratum may only correct a name or
syntax in a printed query or command (or an instruction that cannot be executed as written), justified from
`db/migrations` and source text, never from query output. It never adds, removes or loosens a predicate, join, bin,
sort key or tie-break, and never touches a number, a population, an estimator, a threshold, a split, the three
choices, what counts as a look, the trials table or the outcomes; any of those is a new protocol. A change to
`cmd/measure3` after its first prod run that alters a query, a filter or an arithmetic step is entered here too.
Each entry: the text replaced and its replacement; the text that governs; why no decision can change; the owner's yes;
after any figure has been seen, the error text that made it necessary. Its date is its commit's. Committed and pushed
before the `measure3` run it affects.

No errata yet.
