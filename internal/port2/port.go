// Licensed Materials - Property of IBM
// Copyright IBM Corp. 2023.
// US Government Users Restricted Rights - Use, duplication or disclosure restricted by GSA ADP Schedule Contract with IBM Corp.

package port2

import (
	"fmt"
	"go/parser"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"github.com/zosopentools/wharf/internal/base"
	"github.com/zosopentools/wharf/internal/pkg2"
	"github.com/zosopentools/wharf/internal/tags"
	"github.com/zosopentools/wharf/internal/util"
)

// Comments added to files that are altered by Wharf
const (
	_TAG_NOTICE     = "Tags altered by Wharf (added %v)"
	_FILE_NOTICE    = "This file was generated by Wharf (original %v)"
	_USE_NOTICE     = "Imported by Wharf (version %v)"
	_REPLACE_NOTICE = "Added by Wharf"
)

var (
	// Machine readable actions
	// TODO: refactor so that this isn't generated at the end
	ModuleActions  []ModuleAction
	PackageActions []PackageAction
)

var SuppressOutput = false

type PortingError struct {
	Package *pkg2.Package
	Error   error
}

type Controller struct {
	paths  []string
	tree   [][]*pkg2.Package
	states map[*pkg2.Package]*state

	patchable map[*pkg2.Package]bool
	workspace map[string]*workEdit

	// Errors that occurred during porting of a package
	Errors []error

	// Control
	treeIsDirty bool

	// Ensure controller is only ran once
	complete bool

	// Metrics
	loadCount uint
}

func NewController(paths []string) *Controller {
	return &Controller{
		paths:  paths,
		states: make(map[*pkg2.Package]*state),

		patchable: make(map[*pkg2.Package]bool),
		workspace: make(map[string]*workEdit),
	}
}

func (c *Controller) Run() error {
	if c.complete {
		panic("cannot run a controller more than once")
	}
	defer func() {
		c.complete = true
	}()

	// c.treeIsDirty = true

	for {
		if err := c.load(); err != nil {
			return err
		}

		if err := c.portAll(); err != nil {
			return err
		}

		if !c.treeIsDirty {
			break
		}
	}

	return nil
}

func (c *Controller) load() error {
	c.loadCount += 1
	c.treeIsDirty = false

	// Load packages
	targets, err := pkg2.List(c.paths)
	if err != nil {
		return fmt.Errorf("package discovery: %w", err)
	}

	c.tree, err = pkg2.Resolve(targets, func(pkg *pkg2.Package) {
		s := c.states[pkg]
		if s == nil {
			s = &state{}
			c.states[pkg] = s
			if pkg.Meta.Goroot || pkg.Meta.Standard || (pkg2.IsGolangXPkg(pkg) && pkg.Meta.Module.Replace != nil) {
				s.ps = psExhausted
			}
		} else if c.loadCount > 1 {
			// Sanity checks
			if !pkg.FirstLoad && (pkg.Meta.Goroot || pkg.Meta.Standard) && (pkg.Dirty || pkg.DepDirty) {
				// fmt.Fprintf(os.Stderr, "%v", pkg.Meta.ImportPath)
				panic("package found in GOROOT changed after first load")
			}
			// TODO: add a check to see if a golang/x/... pkg was marked dirty when it shouldn't have
		}

		if s.ps == psUnknown && pkg.Included {
			// We do a full build of all included packages
			s.ps = psBuilt
		}

		if pkg.Dirty || pkg.DepDirty {
			if len(pkg.Builds[s.cfi].Syntax) == 0 {
				s.types = types.NewPackage(pkg.Meta.ImportPath, pkg.Meta.Name)
				s.types.MarkComplete()
			} else {
				tcfg := &types.Config{
					IgnoreFuncBodies: !pkg.Included || pkg2.IsStdlibPkg(pkg),
					FakeImportC:      true,
				}
				s.types, s.errs = c.typeCheck(pkg, tcfg)
			}
		}

	})
	if err != nil {
		return fmt.Errorf("build import tree: %w", err)
	}

	return nil
}

func (c *Controller) portAll() error {
	if c.tree == nil {
		panic("package tree not initialized")
	}

	valid := true
	for i := range c.tree {
		packages := c.tree[len(c.tree)-(i+1)]
		// fmt.Fprintf(os.Stderr, "# LAYER %v\n", len(c.tree)-(i+1))

		for _, pkg := range packages {
			state := c.states[pkg]
			// ops := state.ps

			// Skip exhausted or inactive packages
			if (!pkg.Included && state.ps == psUnknown) || state.ps >= psExhausted {
				// fmt.Fprintf(os.Stderr, "SKIP\n")
				continue
			}

			err := c.port(pkg)
			if err != nil {
				fmt.Printf("Package require manual porting: %v\n\t%v\n", pkg.Meta.ImportPath, err.Error())
				return err
			}

			// fmt.Fprintf(os.Stderr, "%v: %v -> %v\n", pkg.Meta.ImportPath, ops, state.ps)

			// TODO: move this logic to the port routine
			if state.ps == psPatched {
				if c.workspace[pkg.Meta.Module.Path] != nil {
					c.workspace[pkg.Meta.Module.Path].action = modImported
				}
				c.patchable[pkg] = true
				pkg.Modified = true
			} else if state.ps < psExhausted {
				valid = false
				pkg.Modified = true
			}

			if c.treeIsDirty {
				return nil
			}
		}

	}

	// TODO: we shouldn't redo a load on a not valid config
	// improve logic to just recheck the "waiting" packages
	if !valid {
		c.treeIsDirty = true
	}

	return nil
}

// Run the build + port process on a package
func (c *Controller) port(pkg *pkg2.Package) error {
	state := c.states[pkg]
	if state == nil {
		panic("no state associated with package under port")
	}

	if c.patchable[pkg] {
		panic("trying to port package that already has patch associated with it")
	}

	fmt.Fprintln(os.Stderr, pkg)
	fmt.Fprintln(os.Stderr, state.ps)
	fmt.Fprintln(os.Stderr, state.errs)
	fmt.Fprintln(os.Stderr, state.cfi)

	// If this is the first time checking this package verify
	// that it has errors before we begin our investigation
	imports := make(map[*pkg2.Package]bool, 0)
	needTag := state.ps == psBrokeParent
	if state.ps == psUnknown {
		tcfg := &types.Config{
			FakeImportC: true,
		}

		state.types, state.errs = c.typeCheck(pkg, tcfg)
		state.ps = psBuilt
	}

	if state.ps == psBuilt {
		if len(state.errs) > 0 && !SuppressOutput {
			fmt.Println("Build errors occurred in:", pkg.Meta.ImportPath)
		}

		var illList []pkg2.TypeError
		for _, err := range state.errs {
			if iname, ok := err.Reason.(pkg2.TCBadImportName); ok {
				fname := err.Err.Fset.Position(err.Err.Pos).Filename
				file := pkg.Files[fname]
				if file.Imports[iname.PkgName] != "" {
					imports[pkg.Imports[file.Imports[iname.PkgName]]] = true
				} else if backup := pkg2.BackupNameLookup(iname.PkgName); backup != nil {
					imports[backup] = true
				} else {
					fmt.Fprintln(os.Stderr, iname.PkgName)
					fmt.Fprintln(os.Stderr, err.Err.Error())
					panic("bad import on unknown package")
				}
			} else if _, ok := err.Reason.(pkg2.TCBadName); ok {
				needTag = true
			} else {
				illList = append(illList, err)
			}

			if base.Verbose {
				fmt.Println("\t" + err.Err.Error())
			}
		}

		// If we saw no errors, move on
		if !needTag && len(imports) == 0 {
			state.ps = psValid
			return nil
		}

		// Never try porting a package with unknown type errors
		if len(illList) > 0 {
			return fmt.Errorf("unknown type error(s) occurred in %v: %v", pkg.Meta.ImportPath, illList)
		}
	}

	// First lock the version the module will use (using the following process):
	//
	// 1. If the module can be updated we try locking it to the updated version
	// 2. If we already tried the updated version then lock it to the original version determined by MVS
	if mptc := c.workspace[pkg.Meta.Module.Path]; !pkg.Meta.Module.Main && ((mptc == nil && pkg.Meta.Module.Replace == nil) || (mptc != nil && mptc.action < modLocked)) {
		// Entering this point we should only have the following cases:
		// - Modules in the mod cache that haven't been updated or locked
		// - golang.org/x/... modules that haven't been updated
		mod := pkg.Meta.Module
		version := mod.Version
		// Used to track if we change the version we loaded
		updated := false

		if mptc == nil {
			mptc = &workEdit{
				original: mod.Version,
				version:  mod.Version,
				action:   modLocked,
			}
			c.workspace[pkg.Meta.Module.Path] = mptc

			uver, err := util.GoListModUpdate(mod.Path)
			if err != nil && !pkg2.IsExcludeGoListError(err.Error()) {
				return err
			}

			updated = version != uver

			if updated {
				version = uver
				mptc.action = modUpdated
				mptc.version = uver
			}
		} else {
			updated = true
			mptc.action = modLocked
			mptc.version = mod.Version
		}

		if err := util.GoWorkEditReplaceVersion(
			mod.Path,
			version,
		); err != nil {
			return err
		}

		c.treeIsDirty = updated

		// Exhaust golang.org/x/... packages
		if pkg2.IsGolangXPkg(pkg) {
			state.ps = psExhausted
			return nil
		}

		if updated {
			state.ps = psUnknown
			return nil
		}
	}

	// Have to do tagging
	if needTag {
		// Use the error filter to rebuild the import list whenever we decide to use a new config
		errIdx := 1
		state.cfi = errIdx
		c.filterConfigs(pkg, func(err2 pkg2.TypeError) bool {
			if errIdx < state.cfi {
				imports = make(map[*pkg2.Package]bool)
			}

			if iname, ok := err2.Reason.(pkg2.TCBadImportName); ok && imports != nil {
				file := pkg.Files[err2.Err.Fset.Position(err2.Err.Pos).Filename]
				if file.Imports[iname.PkgName] != "" {
					imports[pkg.Imports[file.Imports[iname.PkgName]]] = true
				} else if backup := pkg2.BackupNameLookup(iname.PkgName); backup != nil {
					imports[backup] = true
				} else {
					fmt.Fprintln(os.Stderr, err2.Err.Error())
					panic("bad import on unknown package")
				}
				return true
			}

			if !err2.Err.Soft && imports != nil {
				imports = nil
				return false
			}

			return err2.Err.Soft
		})

		if state.cfi >= len(pkg.Builds) {
			return fmt.Errorf("unable to find a valid config")
		}

		if len(imports) == 0 {
			state.ps = psPatched
			// TODO: make patch
			return nil
		}

	}

	// We have reached the port dependencies stage

	// If we have have come back here after doing a run through the dependencies we should recheck the build
	if state.ps == psPortingDependencies {
		for _, err := range state.errs {
			if info, ok := err.Reason.(pkg2.TCBadImportName); ok {
				file := pkg.Files[err.Err.Fset.Position(err.Err.Pos).Filename]
				if file.Imports[info.PkgName] != "" {
					imports[pkg.Imports[file.Imports[info.PkgName]]] = true
				} else if backup := pkg2.BackupNameLookup(info.PkgName); backup != nil {
					imports[backup] = true
				} else {
					panic("bad import on unknown package")
				}
			} else {
				fmt.Fprintln(os.Stderr, err.Err)
				panic("unsanitized config used for doing porting of depenendencies")
			}
		}
	} else if len(imports) == 0 {
		panic("advancing to dependency porting but no bad dependencies found")
	}

	// If we fixed all the dependencies we can say this ported
	if len(imports) == 0 {
		if state.cfi == 0 {
			state.ps = psValid
		} else {
			state.ps = psPatched
		}
		// make patch
		return nil
	}

	// Keep track if any imports are portable
	canPortImports := false
	for ipkg := range imports {
		istate := c.states[ipkg]

		if istate.ps < psExhausted {
			canPortImports = true
			if istate.ps < psBrokeParent {
				istate.ps = psBrokeParent
			}
		} else if istate.ps == psPatched {
			panic("package is claimed to be patchable but has bad parent")
		}
	}

	// Try porting imports first if possible
	if canPortImports {
		state.ps = psPortingDependencies
		return nil
	}

	// Try retagging to remove the bad imports
	fiEdits := make(fileImportEdits)
	expIdx := state.cfi
	c.filterConfigs(pkg, func(te pkg2.TypeError) bool {
		if expIdx < state.cfi && fiEdits == nil {
			expIdx = state.cfi
			fiEdits = make(fileImportEdits)
		}

		if info, ok := te.Reason.(pkg2.TCBadImportName); ok && fiEdits != nil {
			// If we haven't already found a build we can fix by changing imports
			// check all the errors on the current build to see if they could be changed
			fname := te.Err.Fset.Position(te.Err.Pos).Filename
			ipath := pkg.Files[fname].Imports[info.PkgName]
			directives := base.Inlines[ipath]
			if directives != nil && directives.Exports != nil {
				ed, ok := directives.Exports[info.Name.Name]
				if ok {
					if fiEdits[fname] == nil {
						fiEdits[fname] = make(map[string]map[string]base.ExportInline)
						fiEdits[fname][info.PkgName] = make(map[string]base.ExportInline)
					} else if fiEdits[fname][info.PkgName] == nil {
						fiEdits[fname][info.PkgName] = make(map[string]base.ExportInline)
					}

					fiEdits[fname][info.PkgName][info.Name.Name] = ed
					return false
				}
			}
		}

		if !te.Err.Soft && expIdx == state.cfi {
			// Unreplaceable error, therefore disregard this config
			fiEdits = nil
			return false
		}

		return te.Err.Soft
	})

	if state.cfi < len(pkg.Builds) {
		state.ps = psPatched
		return nil
	}

	pkgCacheDir := filepath.Join(base.Cache, pkg.Meta.ImportPath)
	if err := os.MkdirAll(pkgCacheDir, 0740); err != nil {
		return fmt.Errorf("unable to create cache directory for package: %w", err)
	}

	// Didn't find a working config, therefore we try to use export directives
	if fiEdits != nil {
		state.cfi = expIdx
		err := c.applyExportDirective(pkg, pkgCacheDir, fiEdits)
		if err == nil {
			state.ps = psPatched
			return nil
		}
	}

	// We couldn't find a working config, so we try and see if we can fix the package using explicit handlers
	//
	// TODO: RE-IMPLEMENT THIS
	// if handler := base.Inlines[pkg.Meta.ImportPath]; handler != nil && handler.Files != nil {
	// 	err := c.applyPackageDirective(pkg, pkgCacheDir, handler.Files)
	// 	// TODO: report on diff failing
	// 	if err == nil {
	// 		state.ps = psPatched
	// 		return nil
	// 	}
	// }

	state.ps = psExhausted
	return fmt.Errorf("no applicable options available to port package %v", pkg.Meta.ImportPath)
}

func (c *Controller) Apply() error {
	if !c.complete {
		panic("trying to apply incomplete porting job")
	}
	showActions := (base.Verbose || base.DryRun) && !SuppressOutput
	makeDiff := !base.DryRun && base.GeneratePatches
	diffs := make(map[string]bool)
	ModuleActions = make([]ModuleAction, 0, len(c.workspace))
	PackageActions = make([]PackageAction, 0, 10)

	// fmt.Fprintf(os.Stderr, "workspace: %v", c.workspace)
	// fmt.Fprintf(os.Stderr, "patchable: %v", c.patchable)

	for path, ptc := range c.workspace {
		var action ModuleAction
		action.Path = path
		action.Version = ptc.original
		action.Fixed = ptc.version
		action.Imported = ptc.action == modImported
		if action.Imported {
			action.Dir = filepath.Join(base.ImportDir, path)
		}
		ModuleActions = append(ModuleActions, action)
	}

	// TODO change this to be printed at the end
	if !SuppressOutput {
		for path, ptc := range c.workspace {
			fmt.Printf("%v %v ", path, ptc.version)
			switch ptc.action {
			case modUpdated:
				fmt.Print("(UPDATED)")
			case modLocked:
				fmt.Print("(LOCKED)")
			case modImported:
				fmt.Print("(IMPORTED)")
			default:
				panic("unknown mod action")
			}
			fmt.Println()
		}
	}

	for pkg := range c.patchable {
		state := c.states[pkg]
		if !SuppressOutput {
			fmt.Println("#", pkg.Meta.ImportPath)
		}
		var action PackageAction
		action.Path = pkg.Meta.ImportPath
		action.Module = pkg.Meta.Module.Path
		action.Dir = pkg.Meta.Dir

		if makeDiff {
			diffs[pkg.Meta.Module.Dir] = true
		}

		if state.cfi == 0 {
			panic("trying to patch using default config (no changes)")
		}

		// Perform a clone to workspace if necessary
		if !pkg.Meta.Module.Main {
			path := filepath.Join(base.ImportDir, pkg.Meta.Module.Path)
			action.Dir = path

			if base.CloneFromVCS {
				if err := util.CloneModuleFromVCS(
					path,
					pkg.Meta.Module.Path,
					strings.TrimSuffix(pkg.Meta.Module.Version, "+incompatible"),
				); err != nil {
					return err
				}
			} else {
				if err := util.CloneModuleFromCache(pkg.Meta.Module.Dir, path, pkg.Meta.Module.Path); err != nil {
					return err
				}
			}

			if err := util.GoWorkEditDropReplace(pkg.Meta.Module.Path); err != nil {
				return err
			}

			// TODO: Go work use fails silently on a missing go.mod file, rerun 'go list' to verify it is now a main module position has changed
			if err := util.GoWorkUse(path); err != nil {
				return err
			}

			err := util.GoListModMain(pkg.Meta.Module.Path)
			if err != nil && !pkg2.IsExcludeGoListError(err.Error()) {
				return err
			}
			pkg.Meta.Module.Main = true
		}

		// TODO: find a better way to get the package directory
		dir, err := util.GoListPkgDir(pkg.Meta.ImportPath)
		if err != nil {
			if pkg2.IsExcludeGoListError(err.Error()) {
				dir = filepath.Join(base.ImportDir, pkg.Meta.Module.Path)
			} else {
				return err
			}
		}

		dcfg := pkg.Builds[0]
		pcfg := pkg.Builds[state.cfi]

		// Print out description of patch
		// Platform == 0 (default) when we apply a manual patch
		if len(pcfg.Platforms) == 0 {
			if !SuppressOutput {
				fmt.Println("Applied manual patch")
			}

			for _, gofile := range pcfg.Files {
				if gofile.Replaced != nil {
					util.CopyFile(filepath.Join(dir, gofile.Name), gofile.Path)
				}
			}
		} else {
			action.Tags = pcfg.Platforms[:]

			if !SuppressOutput {
				fmt.Print("Applying tags to match platform(s): ")
				for pidx, pltf := range pcfg.Platforms {
					fmt.Print(pltf)
					if pidx < len(pcfg.Platforms)-1 {
						fmt.Print(", ")
					}
				}
				fmt.Println()
			}

			// Mark the files that were active in the default config
			current := make(map[*pkg2.GoFile]bool)
			for _, gofile := range dcfg.Files {
				current[gofile] = true
			}

			// Apply changes to files that were changed
			for _, gofile := range pcfg.Files {
				var fileAction FileAction
				fileAction.Name = gofile.Name
				fileAction.Build = !current[gofile]

				if gofile.Replaced != nil {
					action.Files = append(action.Files, fileAction)
					repl := gofile.Replaced.File
					// Copy overriden files from the cache
					fileAction.Name = repl.Name
					fileAction.Build = !current[repl]
					var ovrFileAction FileAction
					ovrFileAction.BaseFile = repl.Name
					ovrFileAction.Name = gofile.Name
					ovrFileAction.Build = true
					action.Files = append(action.Files, ovrFileAction)

					if showActions {
						fmt.Printf("%v: copied to %v\n", repl.Name, gofile.Name)
					}

					if current[gofile] {
						action.Files = append(action.Files, fileAction)
						if showActions {
							fmt.Printf("%v: added tag '!%v'\n", repl.Name, base.GOOS())
						}

						if !base.DryRun {
							err := util.CopyFile(filepath.Join(dir, gofile.Name), gofile.Path)
							if err != nil {
								return err
							}

							src, err := util.Format(repl.Syntax, pkg2.FileSet)
							if err != nil {
								return err
							}

							src, err = util.AppendTagString(src, "!"+base.GOOS(), "&&", fmt.Sprintf(_TAG_NOTICE, "!"+base.GOOS()))
							if err != nil {
								return err
							}

							err = os.WriteFile(filepath.Join(dir, repl.Name), src, 0744)
							if err != nil {
								return err
							}
						}
					}

					if !base.DryRun {
						src, err := util.Format(gofile.Syntax, pkg2.FileSet)
						if err != nil {
							return err
						}

						src, err = util.AppendTagString(src, base.GOOS(), "", fmt.Sprintf(_FILE_NOTICE, gofile.Name))
						if err != nil {
							return err
						}

						err = os.WriteFile(filepath.Join(dir, gofile.Name), src, 0744)
						if err != nil {
							return err
						}
					}

					// Describe the change changes
					if showActions {
						fmt.Printf("%v: added tag '%v'\n", gofile.Name, base.GOOS())
					}
					for iname, symbols := range gofile.Replaced.Reason.(map[string]map[string]base.ExportInline) {
						for symname, ed := range symbols {
							var repstr string
							switch ed.Type {
							case "EXPORT":
								repstr = iname + "." + ed.Replace
							case "CONTSTANT":
								repstr = ed.Replace
							default:
								panic("unknown export directive type")
							}
							action.Tokens = append(action.Tokens, TokenAction{
								File:   gofile.Name,
								Token:  fmt.Sprintf("%v.%v", iname, symname),
								Change: repstr,
							})
							if showActions {
								fmt.Printf("%v: replaced %v.%v with %v\n", gofile.Name, iname, symname, repstr)
							}
						}
					}

					if !showActions && !SuppressOutput {
						fmt.Printf("%v: fixed imports\n", repl.Name)
					}

				} else if !current[gofile] {
					action.Files = append(action.Files, fileAction)
					// Add tags to files that were not in the default config
					if showActions {
						fmt.Printf("%v: added %v tag\n", gofile.Name, base.GOOS())
					}

					// the default config setting does not contain any files
					// this means "build constraints exclude all Go files" (_BUILD_CONSTRAINS_EXCLUDE_ALL_FILE)
					if len(dcfg.Files) == 0 {
						newName := fmt.Sprintf("%v_%v.go", strings.TrimSuffix(gofile.Name, ".go"), base.GOOS())

						if showActions {
							fmt.Printf("%v: copied to %v\n", gofile.Name, newName)
						}

						err := util.CopyFile(filepath.Join(dir, newName), filepath.Join(dir, gofile.Name))
						if err != nil {
							return err
						}
					}

					src, err := util.Format(gofile.Syntax, pkg2.FileSet)
					if err != nil {
						return err
					}

					src, err = util.AppendTagString(src, base.GOOS(), "||", fmt.Sprintf(_TAG_NOTICE, base.GOOS()))
					if err != nil {
						return err
					}

					name := gofile.Name
					cnstr, _ := tags.ParseFileName(name)
					if cnstr != nil {
						name = strings.TrimSuffix(name, ".go") + "_" + base.GOOS() + ".go"
					}

					err = os.WriteFile(filepath.Join(dir, name), src, 0744)
					if err != nil {
						return err
					}
				}

				delete(current, gofile)
			}

			// Any files that we have left and aren't seen in the current config, we tag to exclude
			for gofile := range current {
				var fileAction FileAction
				fileAction.Name = gofile.Name
				fileAction.Build = false
				action.Files = append(action.Files, fileAction)
				if showActions {
					fmt.Printf("%v: added !%v tag\n", gofile.Name, base.GOOS())
				}

				src, err := util.Format(gofile.Syntax, pkg2.FileSet)
				if err != nil {
					return err
				}

				src, err = util.AppendTagString(src, "!"+base.GOOS(), "&&", fmt.Sprintf(_TAG_NOTICE, "!"+base.GOOS()))
				if err != nil {
					return err
				}

				err = os.WriteFile(filepath.Join(dir, gofile.Name), src, 0744)
				if err != nil {
					return err
				}
			}
		}
		PackageActions = append(PackageActions, action)
	}

	if makeDiff && len(diffs) > 0 {
		outdir, _ := filepath.Abs(base.GOWORK())
		outdir = filepath.Dir(outdir)
		for path := range diffs {
			out := filepath.Join(outdir, filepath.Base(path)+".patch")
			if err := util.GitDiff(path, out); err != nil {
				fmt.Fprintf(os.Stderr, "Unable to produce patch file for repo located at %v: %v", path, err.Error())
			}
		}
	}

	return nil
}

// Type check each config until one without issues passes
//
// An optional filter can be provided to filter out which errors
// are to be ignored when determining whether a configuration is valid
func (c *Controller) filterConfigs(pkg *pkg2.Package, filter func(pkg2.TypeError) bool) {
	state := c.states[pkg]
	for state.cfi < len(pkg.Builds) {
		cfg := &pkg.Builds[state.cfi]
		// Make sure we have the syntax loaded
		if cfg.Syntax == nil {
			for _, gofile := range cfg.Files {
				if gofile.Syntax == nil {
					src, err := os.ReadFile(gofile.Path)
					if err != nil {
						// TODO: this is BAD SHOULD NOT BE A PANIC
						panic(err)
					}

					parsed, err := parser.ParseFile(pkg2.FileSet, gofile.Name, src, 0)
					if err != nil {
						// TODO: this is BAD SHOULD NOT BE A PANIC
						panic(err)
					}
					gofile.Syntax = parsed
				}
				cfg.Syntax = append(cfg.Syntax, gofile.Syntax)
			}
		}

		if c.validate(pkg, filter) {
			return
		}
		state.cfi++
	}
}

func (c *Controller) validate(pkg *pkg2.Package, filter func(pkg2.TypeError) bool) bool {
	state := c.states[pkg]

	// Step 1: Build the package we are testing first
	typed, errs := c.typeCheck(pkg, &types.Config{FakeImportC: true})

	for _, err := range errs {
		doPass := err.Err.Soft
		if filter != nil {
			doPass = filter(err)
		}
		if !doPass {
			return false
		}
	}

	state.types = typed
	// Step 2: Build the parent packages using this version of the package
	for _, parent := range pkg.Parents {
		// TODO: apply result of type check to parent upon success
		// TODO: run through parents in order of level (prevent potential mixmatched type errors)
		_, errs := c.typeCheck(parent, &types.Config{FakeImportC: true})

		for _, err := range errs {
			// We only care about errors from local imports
			if info, ok := err.Reason.(pkg2.TCBadImportName); ok {
				ipath, ok := parent.Files[err.Err.Fset.Position(err.Err.Pos).Filename].Imports[info.PkgName]
				if !ok {
					if backup := pkg2.BackupNameLookup(info.PkgName); backup != nil {
						ipath = backup.Meta.ImportPath
					} else {
						panic("bad import error but no known name in lookup")
					}
				}

				// If we have a match then that means the parents failed because of
				// of the package under test, therefore we have a bad build
				if pkg.Meta.ImportPath == ipath {
					return false
				}
			} else if !err.Err.Soft {
				// TODO: handle gracefully (allow cleanup)
				// panic(
				// 	fmt.Sprintf(
				// 		"unsanitized parent package: %v for %v has error %v",
				// 		parent.ImportPath, pkg.Meta.ImportPath,
				// 		err.Err.Error(),
				// 	),
				// )
			}
		}
	}

	return true
}

func (c *Controller) typeCheck(pkg *pkg2.Package, tcfg *types.Config) (typed *types.Package, errs []pkg2.TypeError) {
	s := c.states[pkg]
	if s == nil {
		panic("no state found for package during type check")
	}

	tcfg.Error = func(err error) {
		errs = append(errs, pkg2.NewTypeCheckError(err.(types.Error)))
	}

	tcfg.Importer = (pkg2.Importer)(func(path string) (*types.Package, error) {
		if path == pkg2.UNSAFE_PACKAGE_NAME {
			return types.Unsafe, nil
		}

		ipkg := pkg.Imports[path]
		if ipkg == nil {
			panic("unknown imported package path requested during type check")
		}

		istate := c.states[ipkg]
		if istate == nil {
			panic("imported package with uninitialized state found during type check")
		}

		if istate.types == nil {
			panic("imported package with unintialized types object found during type check")
		}

		return istate.types, nil
	})

	typed, _ = tcfg.Check(pkg.Meta.ImportPath, pkg2.FileSet, pkg.Builds[s.cfi].Syntax, nil)
	return
}