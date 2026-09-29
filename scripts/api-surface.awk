# api-surface.awk reduces `go doc -all <pkg>` to the declaration lines it
# prints, dropping the prose, so two trees can be diffed for the surface a
# consumer compiles against. Doc comments and the package comment change with
# every wording pass and say nothing about the contract; a declaration line is
# the contract.
#
# The section header is the boundary: everything above it is the package
# comment, which go doc prints unindented exactly like a declaration.
#
# A declaration line is printed with its runs of whitespace collapsed to one
# space and its leading space dropped. go doc prints the source as gofmt
# aligned it, so a struct field is padded out to the longest type beside it:
# adding one field renumbers the padding on every neighbour, and an
# unnormalized diff reads each of them as a declaration the release removed.
# None of the padding is part of the contract; the name, the type and the tag
# are.
/^(FUNCTIONS|VARIABLES|TYPES|CONSTANTS|METHODS)$/ { f = 1; next }
!f { next }
/^$/ { next }
/^    / { next }
/^\/\// { next }
/^\t\/\// { next }
/^\(/ { next }
/^\)$/ { next }
/\}$/ { next }
/^(func|type|var|const) \($/ { next }
{ gsub(/[[:space:]]+/, " "); sub(/^ /, ""); print }
