package guard

import "testing"

// TestHarden_Classify is the broad adversarial table: every false-positive narrowing
// and every false-negative write/delete vector, proved at the Classify boundary the
// adapter actually calls — plus the wrapper/quoting robustness and the documented
// interpreter-inline gap asserted as a conscious pass, not a silent surprise.
func TestHarden_Classify(t *testing.T) {
	cases := []struct {
		cmd  string
		want Tier
	}{
		// --- FP narrowings: the secret denies, the look-alike is writable ---------
		{"rm .env", TierDeny},
		{"cp tmpl .env.example", TierPass}, // safe template, normal agent action
		{"cp tmpl .env.sample", TierPass},
		{"tee .env.template", TierPass},
		{"cp tmpl .env.dist", TierPass},
		{"rm id_rsa", TierDeny},
		{"sed -i s/a/b/ id_helper.go", TierCheckpoint}, // ordinary source: sed -i checkpoints, never denies
		{"cp x id_helper.go", TierPass},
		{"cp x id_rsa.pub", TierPass}, // public key: writable

		// --- FN: cp / tee / install / ln to a protected dest all deny -------------
		{"cp x .env", TierDeny},
		{"cp -f secrets.txt .env", TierDeny},
		{"tee .env", TierDeny},
		{"tee -a .npmrc", TierDeny},
		{"install -m600 x .env", TierDeny},
		{"install -m 600 x .env", TierDeny}, // value flag as a separate token
		{"ln -f x .env", TierDeny},
		{"ln -sf /etc/passwd .ssh/authorized_keys", TierDeny}, // .ssh/** linkname
		{`ln /backup/.env`, TierDeny},                         // 1-arg ln: basename .env created in cwd

		// --- FN (review#1): write target escaping the tree denies, parity with >/rm
		{"tee /etc/hosts", TierDeny},
		{"tee ~/.bashrc", TierDeny},
		{"cp x /etc/hosts", TierDeny},
		{"mv x /etc/hosts", TierDeny},
		{"install x ~/.bashrc", TierDeny},
		{"ln -f x /etc/hosts", TierDeny},
		{"cp x .salvager/objects/y", TierDeny}, // … and onto the net
		{"tee .salvager/x", TierDeny},
		{"mv x .salvager/x", TierDeny},

		// … and their non-protected, in-tree forms pass (no Tier-B noise, no new FP) ----
		{"cp x app.go", TierPass},
		{"cp build/a build/b", TierPass},
		{"tee out.log", TierPass},
		{"cp x .", TierPass}, // dest is the tree root dir = write INTO the tree, not destroy it
		{"mv x .", TierPass},
		{"mv /etc/hosts ./local", TierPass},      // reads outside, writes in-tree
		{"ln -s /etc/hosts hostslink", TierPass}, // reads outside, link in-tree
		{"ln -s /etc/hosts", TierPass},           // 1-arg: basename "hosts" not protected
		{"install -m755 bin/tool ./bin/tool", TierPass},
		{"ln -sf a.txt link.txt", TierPass},

		// --- FN: find -exec rm deletes without -delete ---------------------------
		{`find . -name .env -exec rm {} \;`, TierDeny},    // names a protected file
		{`find . -name .env -execdir rm {} \;`, TierDeny}, // -execdir too
		{`find . -path ./conf/.env -exec truncate -s0 {} \;`, TierDeny},
		{`find / -name x -exec rm {} \;`, TierDeny},           // escapes the tree
		{`find . -name *.tmp -exec rm {} \;`, TierCheckpoint}, // destructive but inside, not protected
		{`find . -name *.log -exec mv {} /tmp \;`, TierCheckpoint},
		{`find . -name *.go -exec cat {} \;`, TierPass}, // non-destructive verb: not flagged
		{`find . -name .env`, TierPass},                 // no -delete/-exec: just a search

		// --- redirect coverage: >>, >| clobber-override, fd dups -----------------
		{"cat secrets >| .env", TierDeny},  // >| onto a protected path
		{"cat data >> .npmrc", TierDeny},   // append onto a protected path
		{"echo hi >| out.txt", TierPass},   // >| to an ordinary file
		{"echo hi >> out.txt", TierPass},   // append to an ordinary file
		{"ls missing 2>&1", TierPass},      // fd dup, not a file write
		{"build > log.txt 2>&1", TierPass}, // dup alongside a benign redirect

		// --- wrapper / quoting / compound robustness -----------------------------
		{"sudo cp x .env", TierDeny},
		{"bash -c 'cp x .env'", TierDeny},
		{`cp x ".env"`, TierDeny},       // quoted dest
		{"true && cp x .env", TierDeny}, // compound: most-severe clause wins
		{"echo $(tee .env)", TierDeny},  // substitution lifted and judged

		// --- documented limit: interpreter-inline writes PASS (conscious gap) -----
		{`python -c "open('.env','w').write(x)"`, TierPass},
		{`node -e "fs.writeFileSync('.env','x')"`, TierPass},
		{`perl -e 'open(F,">",".env")'`, TierPass},
	}
	for _, c := range cases {
		if got := Classify(Request{Tool: "Bash", Command: c.cmd, Root: root}).Tier; got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}
