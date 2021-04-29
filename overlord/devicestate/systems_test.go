// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2021 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package devicestate_test

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/bootloader"
	"github.com/snapcore/snapd/bootloader/bootloadertest"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord/devicestate"
	"github.com/snapcore/snapd/seed"
	"github.com/snapcore/snapd/seed/seedtest"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/snaptest"
	"github.com/snapcore/snapd/testutil"
	"github.com/snapcore/snapd/timings"
)

type createSystemSuite struct {
	deviceMgrBaseSuite

	ss *seedtest.SeedSnaps

	logbuf *bytes.Buffer
}

var _ = Suite(&createSystemSuite{})

var (
	genericSnapYaml = "name: %s\nversion: 1.0\n%s"
	snapYamls       = map[string]string{
		"pc-kernel":        "name: pc-kernel\nversion: 1.0\ntype: kernel",
		"pc":               "name: pc\nversion: 1.0\ntype: gadget\nbase: core20",
		"core20":           "name: core20\nversion: 20.1\ntype: base",
		"core18":           "name: core18\nversion: 18.1\ntype: base",
		"snapd":            "name: snapd\nversion: 2.2.2\ntype: snapd",
		"other-required":   fmt.Sprintf(genericSnapYaml, "other-required", "base: core20"),
		"other-present":    fmt.Sprintf(genericSnapYaml, "other-present", "base: core20"),
		"other-core18":     fmt.Sprintf(genericSnapYaml, "other-present", "base: core18"),
		"other-unasserted": fmt.Sprintf(genericSnapYaml, "other-unasserted", "base: core20"),
	}
	snapFiles = map[string][][]string{
		"pc": {
			{"meta/gadget.yaml", gadgetYaml},
			{"cmdline.extra", "args from gadget"},
		},
	}
)

func (s *createSystemSuite) SetUpTest(c *C) {
	s.deviceMgrBaseSuite.SetUpTest(c)

	s.ss = &seedtest.SeedSnaps{
		StoreSigning: s.storeSigning,
		Brands:       s.brands,
	}
	s.AddCleanup(func() { bootloader.Force(nil) })

	buf, restore := logger.MockLogger()
	s.AddCleanup(restore)
	s.logbuf = buf
}

func (s *createSystemSuite) makeSnap(c *C, name string, rev snap.Revision) *snap.Info {
	snapID := s.ss.AssertedSnapID(name)
	if rev.Unset() || rev.Local() {
		snapID = ""
	}
	si := &snap.SideInfo{
		RealName: name,
		SnapID:   snapID,
		Revision: rev,
	}
	// asserted?
	where, info := snaptest.MakeTestSnapInfoWithFiles(c, snapYamls[name], snapFiles[name], si)
	c.Assert(os.MkdirAll(filepath.Dir(info.MountFile()), 0755), IsNil)
	c.Assert(os.Rename(where, info.MountFile()), IsNil)
	if !rev.Unset() && !rev.Local() {
		s.setupSnapDecl(c, info, "my-brand")
		s.setupSnapRevision(c, info, "my-brand", rev)
	}
	return info
}

func (s *createSystemSuite) validateSeed(c *C, name string) {
	tm := &timings.Timings{}
	db, err := asserts.OpenDatabase(&asserts.DatabaseConfig{
		Backstore: asserts.NewMemoryBackstore(),
		Trusted:   s.storeSigning.Trusted,
	})
	c.Assert(err, IsNil)
	commitTo := func(b *asserts.Batch) error {
		return b.CommitTo(db, nil)
	}

	sd, err := seed.Open(boot.InitramfsUbuntuSeedDir, "1234")
	c.Assert(err, IsNil)

	err = sd.LoadAssertions(db, commitTo)
	c.Assert(err, IsNil)

	err = sd.LoadMeta(tm)
	c.Assert(err, IsNil)
	// uc20 recovery systems use snapd
	c.Check(sd.UsesSnapdSnap(), Equals, true)
	// XXX: more extensive seed validation?
}

func (s *createSystemSuite) TestCreateSystemFromAssertedSnaps(c *C) {
	bl := bootloadertest.Mock("trusted", c.MkDir()).WithRecoveryAwareTrustedAssets()
	// make it simple for now, no assets
	bl.TrustedAssetsList = nil
	bl.StaticCommandLine = "mock static"
	bl.CandidateStaticCommandLine = "unused"
	bootloader.Force(bl)
	infos := map[string]*snap.Info{}

	s.state.Lock()
	defer s.state.Unlock()
	s.setupBrands(c)
	infos["pc-kernel"] = s.makeSnap(c, "pc-kernel", snap.R(1))
	infos["pc"] = s.makeSnap(c, "pc", snap.R(2))
	infos["core20"] = s.makeSnap(c, "core20", snap.R(3))
	infos["snapd"] = s.makeSnap(c, "snapd", snap.R(4))
	infos["other-present"] = s.makeSnap(c, "other-present", snap.R(5))
	infos["other-required"] = s.makeSnap(c, "other-required", snap.R(6))
	infos["other-core18"] = s.makeSnap(c, "other-core18", snap.R(7))
	infos["core18"] = s.makeSnap(c, "core18", snap.R(8))

	model := s.makeModelAssertionInState(c, "my-brand", "pc", map[string]interface{}{
		"architecture": "amd64",
		"grade":        "dangerous",
		"base":         "core20",
		"snaps": []interface{}{
			map[string]interface{}{
				"name":            "pc-kernel",
				"id":              s.ss.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name":            "pc",
				"id":              s.ss.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name": "snapd",
				"id":   s.ss.AssertedSnapID("snapd"),
				"type": "snapd",
			},
			// optional but not present
			map[string]interface{}{
				"name":     "other-not-present",
				"id":       s.ss.AssertedSnapID("other-not-present"),
				"presence": "optional",
			},
			// optional and present
			map[string]interface{}{
				"name":     "other-present",
				"id":       s.ss.AssertedSnapID("other-present"),
				"presence": "optional",
			},
			// required
			map[string]interface{}{
				"name":     "other-required",
				"id":       s.ss.AssertedSnapID("other-required"),
				"presence": "required",
			},
			// different base
			map[string]interface{}{
				"name": "other-core18",
				"id":   s.ss.AssertedSnapID("other-core18"),
			},
			// and the actual base for that snap
			map[string]interface{}{
				"name": "core18",
				"id":   s.ss.AssertedSnapID("core18"),
				"type": "base",
			},
		},
	})

	infoGetter := func(name string) (*snap.Info, bool, error) {
		c.Logf("called for: %q", name)
		info, present := infos[name]
		return info, present, nil
	}

	newFiles, dir, err := devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, IsNil)
	c.Check(newFiles, DeepEquals, []string{
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/snapd_4.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/pc-kernel_1.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/core20_3.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/pc_2.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/other-present_5.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/other-required_6.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/other-core18_7.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/core18_8.snap"),
	})
	c.Check(dir, Equals, filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234"))
	// naive check for files being present
	for _, info := range infos {
		c.Check(filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps", filepath.Base(info.MountFile())),
			testutil.FileEquals,
			testutil.FileContentRef(info.MountFile()))
	}
	// recovery system bootenv was set
	c.Check(bl.RecoverySystemDir, Equals, "/systems/1234")
	c.Check(bl.RecoverySystemBootVars, DeepEquals, map[string]string{
		"snapd_full_cmdline_args":  "",
		"snapd_extra_cmdline_args": "args from gadget",
		"snapd_recovery_kernel":    "/snaps/pc-kernel_1.snap",
	})
	// load the seed
	s.validateSeed(c, "1234")
}

func (s *createSystemSuite) TestCreateSystemFromUnassertedSnaps(c *C) {
	bl := bootloadertest.Mock("trusted", c.MkDir()).WithRecoveryAwareTrustedAssets()
	// make it simple for now, no assets
	bl.TrustedAssetsList = nil
	bl.StaticCommandLine = "mock static"
	bl.CandidateStaticCommandLine = "unused"
	bootloader.Force(bl)
	infos := map[string]*snap.Info{}

	s.state.Lock()
	defer s.state.Unlock()
	s.setupBrands(c)
	infos["pc-kernel"] = s.makeSnap(c, "pc-kernel", snap.R(1))
	infos["pc"] = s.makeSnap(c, "pc", snap.R(2))
	infos["core20"] = s.makeSnap(c, "core20", snap.R(3))
	infos["snapd"] = s.makeSnap(c, "snapd", snap.R(4))
	// unasserted with local revision
	infos["other-unasserted"] = s.makeSnap(c, "other-unasserted", snap.R(-1))

	model := s.makeModelAssertionInState(c, "my-brand", "pc", map[string]interface{}{
		"architecture": "amd64",
		"grade":        "dangerous",
		"base":         "core20",
		"snaps": []interface{}{
			map[string]interface{}{
				"name":            "pc-kernel",
				"id":              s.ss.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name":            "pc",
				"id":              s.ss.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name": "snapd",
				"id":   s.ss.AssertedSnapID("snapd"),
				"type": "snapd",
			},
			// required
			map[string]interface{}{
				"name":     "other-unasserted",
				"presence": "required",
			},
		},
	})

	infoGetter := func(name string) (*snap.Info, bool, error) {
		c.Logf("called for: %q", name)
		info, present := infos[name]
		return info, present, nil
	}

	newFiles, dir, err := devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, IsNil)
	c.Check(newFiles, DeepEquals, []string{
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/snapd_4.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/pc-kernel_1.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/core20_3.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/pc_2.snap"),
		// this snap unasserted and lands under the system
		filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234/snaps/other-unasserted_1.0.snap"),
	})
	c.Check(dir, Equals, filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234"))
	// naive check for files being present
	for _, info := range infos {
		if info.Revision.Store() {
			c.Check(filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps", filepath.Base(info.MountFile())),
				testutil.FileEquals,
				testutil.FileContentRef(info.MountFile()))
		} else {
			fileName := fmt.Sprintf("%s_%s.snap", info.SnapName(), info.Version)
			c.Check(filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234/snaps", fileName),
				testutil.FileEquals,
				testutil.FileContentRef(info.MountFile()))
		}
	}
	// load the seed
	s.validateSeed(c, "1234")
	// we have unasserted snaps, so a warning should have been logged
	c.Check(s.logbuf.String(), testutil.Contains, `system "1234" contains unasserted snaps "other-unasserted"`)
}

func (s *createSystemSuite) TestCreateSystemWithSomeSnapsAlreadyExisting(c *C) {
	bl := bootloadertest.Mock("trusted", c.MkDir()).WithRecoveryAwareTrustedAssets()
	bootloader.Force(bl)
	infos := map[string]*snap.Info{}

	s.state.Lock()
	defer s.state.Unlock()
	s.setupBrands(c)
	infos["pc-kernel"] = s.makeSnap(c, "pc-kernel", snap.R(1))
	infos["pc"] = s.makeSnap(c, "pc", snap.R(2))
	infos["core20"] = s.makeSnap(c, "core20", snap.R(3))
	infos["snapd"] = s.makeSnap(c, "snapd", snap.R(4))
	model := s.makeModelAssertionInState(c, "my-brand", "pc", map[string]interface{}{
		"architecture": "amd64",
		"grade":        "dangerous",
		"base":         "core20",
		"snaps": []interface{}{
			map[string]interface{}{
				"name":            "pc-kernel",
				"id":              s.ss.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name":            "pc",
				"id":              s.ss.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name": "snapd",
				"id":   s.ss.AssertedSnapID("snapd"),
				"type": "snapd",
			},
		},
	})

	infoGetter := func(name string) (*snap.Info, bool, error) {
		c.Logf("called for: %q", name)
		info, present := infos[name]
		return info, present, nil
	}

	assertedSnapsDir := filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps")
	c.Assert(os.MkdirAll(assertedSnapsDir, 0755), IsNil)
	// procure the file in place
	err := osutil.CopyFile(infos["core20"].MountFile(), filepath.Join(assertedSnapsDir, "core20_3.snap"), 0)
	c.Assert(err, IsNil)

	// when a given snap in asserted snaps directory already exists, it is
	// not copied over
	newFiles, dir, err := devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, IsNil)
	c.Check(newFiles, DeepEquals, []string{
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/snapd_4.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/pc-kernel_1.snap"),
		filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps/pc_2.snap"),
	})
	c.Check(dir, Equals, filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234"))
	// naive check for files being present
	for _, info := range infos {
		c.Check(filepath.Join(boot.InitramfsUbuntuSeedDir, "snaps", filepath.Base(info.MountFile())),
			testutil.FileEquals,
			testutil.FileContentRef(info.MountFile()))
	}
	// recovery system bootenv was set
	c.Check(bl.RecoverySystemDir, Equals, "/systems/1234")
	c.Check(bl.RecoverySystemBootVars, DeepEquals, map[string]string{
		"snapd_full_cmdline_args":  "",
		"snapd_extra_cmdline_args": "args from gadget",
		"snapd_recovery_kernel":    "/snaps/pc-kernel_1.snap",
	})
	// load the seed
	s.validateSeed(c, "1234")

	//
	infos["other-unasserted"] = s.makeSnap(c, "other-unasserted", snap.R(-1))
	modelWithUnasserted := s.makeModelAssertionInState(c, "my-brand", "pc-with-unasserted", map[string]interface{}{
		"architecture": "amd64",
		"grade":        "dangerous",
		"base":         "core20",
		"snaps": []interface{}{
			map[string]interface{}{
				"name":            "pc-kernel",
				"id":              s.ss.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name":            "pc",
				"id":              s.ss.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name": "snapd",
				"id":   s.ss.AssertedSnapID("snapd"),
				"type": "snapd",
			},
			// required
			map[string]interface{}{
				"name":     "other-unasserted",
				"presence": "required",
			},
		},
	})

	unassertedSnapsDir := filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234unasserted/snaps")
	c.Assert(os.MkdirAll(unassertedSnapsDir, 0755), IsNil)
	err = osutil.CopyFile(infos["other-unasserted"].MountFile(),
		filepath.Join(unassertedSnapsDir, "other-unasserted_1.0.snap"), 0)
	c.Assert(err, IsNil)

	// when a given snap in asserted snaps directory already exists, it is
	// not copied over
	newFiles, dir, err = devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db,
		"1234unasserted", modelWithUnasserted)
	c.Assert(err, ErrorMatches, "unable to create .*/other-unasserted_1.0.snap: file exists")
	c.Check(newFiles, DeepEquals, []string{
		// returned as new file so that cleanup can be properly
		// performed even if the file was partially written
		filepath.Join(unassertedSnapsDir, "other-unasserted_1.0.snap"),
	})
	c.Check(dir, Equals, filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234unasserted"))
	c.Check(osutil.IsDirectory(filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234unasserted")), Equals, true)
}

func (s *createSystemSuite) TestCreateSystemInfoAndAssertsChecks(c *C) {
	bl := bootloadertest.Mock("trusted", c.MkDir()).WithRecoveryAwareTrustedAssets()
	bootloader.Force(bl)
	infos := map[string]*snap.Info{}

	s.state.Lock()
	defer s.state.Unlock()
	s.setupBrands(c)
	// missing info for the pc snap
	infos["pc-kernel"] = s.makeSnap(c, "pc-kernel", snap.R(1))
	infos["core20"] = s.makeSnap(c, "core20", snap.R(3))
	infos["snapd"] = s.makeSnap(c, "snapd", snap.R(4))
	model := s.makeModelAssertionInState(c, "my-brand", "pc", map[string]interface{}{
		"architecture": "amd64",
		"grade":        "dangerous",
		"base":         "core20",
		"snaps": []interface{}{
			map[string]interface{}{
				"name":            "pc-kernel",
				"id":              s.ss.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			// pc snap is the gadget, but we have no info for it
			map[string]interface{}{
				"name":            "pc",
				"id":              s.ss.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name": "snapd",
				"id":   s.ss.AssertedSnapID("snapd"),
				"type": "snapd",
			},
			map[string]interface{}{
				"name":     "other-required",
				"id":       s.ss.AssertedSnapID("other-required"),
				"presence": "required",
			},
		},
	})

	infoGetter := func(name string) (*snap.Info, bool, error) {
		c.Logf("called for: %q", name)
		info, present := infos[name]
		return info, present, nil
	}

	systemDir := filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234")

	// when a given snap in asserted snaps directory already exists, it is
	// not copied over
	newFiles, dir, err := devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, ErrorMatches, `internal error: essential snap "pc" not present`)
	c.Check(newFiles, HasLen, 0)
	c.Check(dir, Equals, "")
	// the directory shouldn't be there, as we haven't written anything yet
	c.Check(osutil.IsDirectory(systemDir), Equals, false)

	// create the info now
	infos["pc"] = s.makeSnap(c, "pc", snap.R(2))

	// and try with with a non essential snap
	newFiles, dir, err = devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, ErrorMatches, `internal error: non-essential but "required" snap "other-required" not present`)
	c.Check(newFiles, HasLen, 0)
	c.Check(dir, Equals, "")
	// the directory shouldn't be there, as we haven't written anything yet
	c.Check(osutil.IsDirectory(systemDir), Equals, false)

	// create the info now
	infos["other-required"] = s.makeSnap(c, "other-required", snap.R(5))

	// but change the file contents of 'pc' snap so that deriving side info fails
	c.Assert(ioutil.WriteFile(infos["pc"].MountFile(), []byte("canary"), 0644), IsNil)
	newFiles, dir, err = devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, ErrorMatches, `internal error: no assertions for asserted snap with ID: pcididididididididididididididid`)
	// we're past the start, so the system directory is there
	c.Check(dir, Equals, systemDir)
	c.Check(osutil.IsDirectory(systemDir), Equals, true)
	// but no files were copied
	c.Check(newFiles, HasLen, 0)
}

func (s *createSystemSuite) TestCreateSystemGetInfoErr(c *C) {
	bl := bootloadertest.Mock("trusted", c.MkDir()).WithRecoveryAwareTrustedAssets()
	bootloader.Force(bl)
	infos := map[string]*snap.Info{}

	s.state.Lock()
	defer s.state.Unlock()
	s.setupBrands(c)
	// missing info for the pc snap
	infos["pc-kernel"] = s.makeSnap(c, "pc-kernel", snap.R(1))
	infos["pc"] = s.makeSnap(c, "pc", snap.R(2))
	infos["core20"] = s.makeSnap(c, "core20", snap.R(3))
	infos["snapd"] = s.makeSnap(c, "snapd", snap.R(4))
	infos["other-required"] = s.makeSnap(c, "other-required", snap.R(5))
	model := s.makeModelAssertionInState(c, "my-brand", "pc", map[string]interface{}{
		"architecture": "amd64",
		"grade":        "dangerous",
		"base":         "core20",
		"snaps": []interface{}{
			map[string]interface{}{
				"name":            "pc-kernel",
				"id":              s.ss.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			// pc snap is the gadget, but we have no info for it
			map[string]interface{}{
				"name":            "pc",
				"id":              s.ss.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			},
			map[string]interface{}{
				"name": "snapd",
				"id":   s.ss.AssertedSnapID("snapd"),
				"type": "snapd",
			},
			map[string]interface{}{
				"name":     "other-required",
				"id":       s.ss.AssertedSnapID("other-required"),
				"presence": "required",
			},
		},
	})

	failOn := map[string]bool{}

	infoGetter := func(name string) (*snap.Info, bool, error) {
		c.Logf("called for: %q", name)
		if failOn[name] {
			return nil, false, fmt.Errorf("mock failure for snap %q", name)
		}
		info, present := infos[name]
		return info, present, nil
	}

	systemDir := filepath.Join(boot.InitramfsUbuntuSeedDir, "systems/1234")

	// when a given snap in asserted snaps directory already exists, it is
	// not copied over

	failOn["pc"] = true
	newFiles, dir, err := devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, ErrorMatches, `cannot obtain snap information: mock failure for snap "pc"`)
	c.Check(newFiles, HasLen, 0)
	c.Check(dir, Equals, "")
	c.Check(osutil.IsDirectory(systemDir), Equals, false)

	failOn["pc"] = false
	failOn["other-required"] = true
	newFiles, dir, err = devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, ErrorMatches, `cannot obtain non-essential snap information: mock failure for snap "other-required"`)
	c.Check(newFiles, HasLen, 0)
	c.Check(dir, Equals, "")
	c.Check(osutil.IsDirectory(systemDir), Equals, false)
}

func (s *createSystemSuite) TestCreateSystemNonUC20(c *C) {
	bl := bootloadertest.Mock("trusted", c.MkDir()).WithRecoveryAwareTrustedAssets()
	bootloader.Force(bl)

	s.state.Lock()
	defer s.state.Unlock()
	s.setupBrands(c)
	model := s.makeModelAssertionInState(c, "my-brand", "pc", map[string]interface{}{
		"architecture": "amd64",
		"base":         "core18",
		"kernel":       "pc-kernel",
		"gadget":       "pc",
	})

	infoGetter := func(name string) (*snap.Info, bool, error) {
		c.Fatalf("unexpected call")
		return nil, false, fmt.Errorf("unexpected call")
	}
	newFiles, dir, err := devicestate.CreateSystemForModelFromValidatedSnaps(infoGetter, s.db, "1234", model)
	c.Assert(err, ErrorMatches, `cannot create a system for non UC20 model`)
	c.Check(newFiles, HasLen, 0)
	c.Check(dir, Equals, "")
}
