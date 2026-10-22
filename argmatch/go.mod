module argmatch

go 1.24

require golang.org/x/tools/gopls/conftamer/stacklog v0.0.0

// The stack log parser shared with conftamer's parse package.
replace golang.org/x/tools/gopls/conftamer/stacklog => ../../golang.org-x-tools/gopls/conftamer/stacklog
