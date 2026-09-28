# api-surface.awk reduces `go doc -all <pkg>` to the declaration lines it
# prints, dropping the prose, so two trees can be diffed for the surface a
# consumer compiles against. Doc comments and the package comment change with
# every wording pass and say nothing about the contract; a declaration line is
# the contract.
#
# The section header is the boundary: everything above it is the package
# comment, which go doc prints unindented exactly like a declaration.
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
{ print }
