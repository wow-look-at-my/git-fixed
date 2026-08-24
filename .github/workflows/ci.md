# ci.yml

## Why the workflow installs git before it runs anything

The differential tests run the system git and compare this implementation against it. A runner whose git is older than `gittest.MinGit` measures
this implementation against a different specification, because an older git rewords its messages. The tests fail rather than skip below that
version, so an old git is a red run, not a quiet one.

Ubuntu's own git is several releases behind, so the workflow takes `ppa:git-core/ppa`, which carries the current release. That lag belongs to the
distribution's packaging and nothing in this repository can remove it, so the reason lives here instead of above the step.
