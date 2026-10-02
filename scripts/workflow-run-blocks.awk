# workflow-run-blocks.awk reduces a GitHub Actions workflow to the bash its
# `run:` blocks contain, so shellcheck can read it. One file per block, named
# <workflow-base>-<line>.bash, written under the -v outdir= directory.
#
# A workflow's steps are the shell code that installs the toolchain, decides
# what a gate runs, and publishes the release. None of it was analyzed: yamllint
# parses the document, biome does not read it, and shellcheck ran only over the
# completion scripts. A quoting slip or an unset variable in a `run:` block is
# therefore a defect that reaches main the way a defect in an unlinted Python
# file would, and the block that installs zsh and fish and then calls
# check-shell is one of them: the failure lands in the one step whose whole job
# is to be sure.
#
# A step writes `run:` either as a mapping key under `- name:` or as a bare list
# item, `- run: |`. Both are read here, because the second is what a step with
# no name is written as, and an extractor that skipped it would leave the common
# form unanalyzed while the gate reported that every block was covered.
#
# The dialect is bash: ci.yml sets `defaults.run.shell: bash` on the job that
# holds these steps, release.yml's blocks are the same bash the rest of the
# tree writes, and bash is what the runner would reject an unbalanced quote in
# anyway. A step whose `run:` is a single line rather than a block scalar is a
# command, not a script, and is skipped: shellcheck reads a one-line file as a
# shebang problem rather than as code.
#
# The header may be `|`, `|-`, `|+`, `>`, `>-`, `>+`, or any of those with a
# digits indicator (`|2`). Chomping changes only the trailing newlines and the
# indicator names the body's column outright instead of deriving it from the
# first content line, so both are followed rather than assumed: a body one space
# deeper than the tree's norm would otherwise lose its last line silently, and a
# half-extracted step is a step half-analyzed. A folded (`>`) block is joined the
# way YAML joins it, because the runner reads one line where the source has
# several, and analyzing a line break that will not be there is analyzing a
# different script.
#
# One file per block also scopes a suppression: a `# shellcheck disable` inside
# a block silences that step alone, where a workflow linted whole would silence
# every other step in the same file. The output lands under dist/, which
# .gitignore covers.
#
# SC2154 (a variable referenced but never assigned) is excluded by the caller
# for these files and not elsewhere: a run block is a fragment of a step, and
# the names it uses come from the job's env block above it rather than from
# anything the block assigns, so the rule would fire on every correctly written
# workflow. Everything else shellcheck ships by default stays on.

BEGIN {
  if (outdir == "") {
    print "workflow-run-blocks.awk: -v outdir=<dir> is required" > "/dev/stderr"
    exit 2
  }
}

function indent_of(s) {
  match(s, /^ */)
  return RLENGTH
}

function is_blank(s) {
  return s ~ /^[ \t]*$/
}

# The column the step's `run` key starts at, or -1 when the line is not a
# `run:` block header. This is the key's own column and not the line's leading
# whitespace: in `- run: |2` the dash is at column 6 and the key at column 8,
# and YAML counts an indentation indicator from the parent node the key belongs
# to. The key's column is also what ends the block, since a body line indented
# no further than the key belongs to whatever comes next.
function header_column(s) {
  if (s !~ /^[ \t]*(-[ \t]+)?run[ \t]*:[ \t]*[|>][0-9]*[+-]?[ \t]*$/) return -1
  match(s, /^ *(-[ \t]+)?/)
  return RLENGTH
}

function workflow_base(path,   n, parts, i) {
  n = split(path, parts, "/")
  return parts[n]
}

# The body of the block whose header is lines[start], as an array of output
# lines in `out`, and the line the block ends at in `end_line`.
function collect(start, col, folded, out,   j, s, ci, text, body_col, indicator, n, space) {
  indicator = 0
  if (match(lines[start], /[|>][0-9]+/)) indicator = substr(lines[start], RSTART + 1, RLENGTH - 1) + 0
  # The body's column, zero-based, so `body_col` is the column the first
  # character of a content line sits in.
  #
  # YAML's own rule, verified against a parser rather than reasoned about: the
  # indicator is counted from the `run` key's column and names an indentation
  # the content has to *exceed*, so the body starts at the first content line
  # when that line is deeper than the key, and at the indicator when the first
  # line lands exactly on it. `run: |2` under a key at column 8 with content at
  # column 10 yields `  echo x`: the two spaces are the `2`, preserved. Reading
  # the indicator as absolute, or counting it from the line's leading
  # whitespace, both put every body columns off, and a body that lost its
  # indentation is a heredoc or a `for` whose body the shell then reads as a
  # command.
  #
  # Without an indicator the first non-blank line after the header decides the
  # column outright, which is the same rule with nothing to add to it.
  body_col = indicator ? col + indicator : -1
  n = 0
  space = 0
  j = start + 1
  while (j <= total) {
    s = lines[j]
    if (is_blank(s)) {
      out[++n] = ""
      space = 0
      j++
      continue
    }
    ci = indent_of(s)
    if (ci <= col) break
    if (body_col < 0) body_col = ci
    if (ci < body_col) break
    # With an indicator, content shallower than it is a syntax error a parser
    # reports and the runner never reaches, so the block ends there rather than
    # handing shellcheck a body the step will not run.
    text = substr(s, body_col + 1)
    # A folded block joins a run of content lines with one space. A line
    # indented deeper than the rest is literal in YAML and keeps its own break,
    # which is how a shell line with leading whitespace survives.
    if (folded) {
      if (ci > body_col || n == 0 || out[n] == "") {
        out[++n] = text
      } else {
        out[n] = out[n] " " text
      }
    } else {
      out[++n] = text
    }
    j++
  }
  end_line = j
  return n
}

{ lines[NR] = $0 }

END {
  if (failed) exit 2
  total = NR
  base = workflow_base(FILENAME)
  i = 1
  while (i <= total) {
    col = header_column(lines[i])
    if (col < 0) {
      i++
      continue
    }
    start = i
    folded = (lines[i] ~ /[>]/) && (lines[i] !~ /^-[ \t]*$/)
    delete body
    n = collect(start, col, folded, body)
    if (n > 0) {
      out = outdir "/" base "-" start ".bash"
      printf("") > out
      printf("#!/bin/bash\n") > out
      printf("# %s line %d\n", FILENAME, start) > out
      for (k = 1; k <= n; k++) printf("%s\n", body[k]) > out
      close(out)
      written++
    }
    i = end_line
  }
  # A workflow with no `run:` block is not an error here: a reusable workflow
  # composed only of `uses:` steps has none, and a workflow whose every step is
  # a one-line `run: make target` has none either. Whether the extraction read
  # anything at all is the caller's question, and it asks it across every
  # workflow at once, so a tree that has lost the extractor entirely fails
  # there rather than one file at a time.
}
