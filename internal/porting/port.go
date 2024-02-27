// Licensed Materials - Property of IBM
// Copyright IBM Corp. 2023.
// US Government Users Restricted Rights - Use, duplication or disclosure restricted by GSA ADP Schedule Contract with IBM Corp.

package porting

import (
	"errors"
	"fmt"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zosopentools/wharf/internal/direct"
	"github.com/zosopentools/wharf/internal/packages"
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
	// Force reloading package hierarchy
	forceLoad bool

	// Keep track of what modules we have updated
	modcache map[string]*modulepatch
)

// The main entry point for porting
//
// Loads and type checks the packages and all dependencies for the given paths
// if any type checking failures occur in any packages, those packages will be attempted to be ported
//
// Config must not be nil
func Port(paths []string, config *Config) (err error) {
	if config == nil {
		panic("config must not be nil")
	}

	if config.GoEnv == nil || len(config.GoEnv) == 0 {
		panic("go environment not initialized")
	}

	// Assumes proper GOOS value
	packages.Goos = config.GoEnv["GOOS"]
	packages.BuildTags = make(map[string]bool, len(config.BuildTags))

	// Default cache location is $GOWORK/.wharf_cache
	if len(config.Cache) == 0 {
		config.Cache = filepath.Join(filepath.Dir(config.GoEnv["GOWORK"]), ".wharf_cache")
	}
	modcache = make(map[string]*modulepatch, 5)

	// Set up build tags list
	err = parseGoEnvTags(config.GoEnv, packages.BuildTags)
	if err != nil {
		return err
	}

	for _, tag := range config.BuildTags {
		packages.BuildTags[tag] = true
	}

	// Initialize the cache
	err = setupCache(config.Cache)
	if err != nil {
		return err
	}

	// Create a temporary go.work file to make changes to
	aGoWork, err := setupTempGoWork(config.GoEnv["GOWORK"])
	if err != nil {
		goto teardown
	}

	// Actually run porting
	err = run(paths, config)

teardown:
	// TODO: only copy go.work if we made changes to the workspace
	if len(aGoWork) > 0 && !config.DryRun {
		backup := config.GoEnv["GOWORK"] + ".backup"
		if werr := util.CopyFile(
			backup,
			config.GoEnv["GOWORK"],
		); werr != nil {
			goto onCopyFail
		}

		fmt.Println("Backed up workspace to", backup)

		if werr := util.CopyFile(
			config.GoEnv["GOWORK"],
			aGoWork,
		); werr != nil {
			goto onCopyFail
		}

		if werr := os.Remove(aGoWork + ".sum"); werr != nil && !errors.Is(werr, fs.ErrNotExist) {
			fmt.Println("WARNING - unable to remove our go.work.sum file:", aGoWork+".sum")
		}

		if werr := os.Remove(aGoWork); werr != nil {
			fmt.Println("WARNING - unable to remove our go.work file:", aGoWork)
		}

		goto onCopyPass

	onCopyFail:
		fmt.Println("An error occurred:")
		fmt.Println("\tUnable to replace the current GOWORK file with our copy.")
		fmt.Println("\tTherefore, some patches might not be applied.")
		fmt.Println("\tOur copy is located here:", aGoWork)

	onCopyPass:
	}

	return err
}

func run(paths []string, cfg *Config) error {
	patchable := make([]*packages.Package, 0, 10)

load:
	// Load packages
	stack, err := packages.Load(paths, func(pkg packages.RawPackage, iscli bool) (packages.LoadOption, error) {
		// Packages that match CLI arguments should always be located inside the workspace
		if iscli && (pkg.Module == nil || !pkg.Module.Main) {
			return 0, fmt.Errorf("%v: target package must be included in Main module", pkg.ImportPath)
		}
		// Goroot and GolangX packages we assume are to be properly typed at all times
		if pkg.Goroot || isGolangX(pkg.Module) {
			return packages.LoadHaltMistyped, nil
		}

		return packages.LoadAllConfigs, nil
	})

	if err != nil {
		// This should rarely occur, the only cases where this might occur are when some
		// other application is altering the workspace as we are trying to work on it
		return fmt.Errorf("unable to load packages: %w", err)
	}

	valid := true
	forceLoad = false

	for {

		for _, pkg := range stack.Packages {
			initProcFlags(pkg)

			// Skip exhausted or inactive packages
			if (!pkg.Active && pkg.ExtFlags == stateUnknown) || pkg.ExtFlags >= stateExhausted {
				continue
			}

			err := port(pkg, cfg)
			if err != nil {
				fmt.Printf("Package require manual porting: %v\n\t%v\n", pkg.ImportPath, err.Error())
				goto apply
			}

			if pkg.ExtFlags == statePatched {
				if modcache[pkg.Module.Path] != nil {
					modcache[pkg.Module.Path].action = modImported
				}
				patchable = append(patchable, pkg)
			} else if pkg.ExtFlags < stateExhausted {
				valid = false
			}

			if forceLoad {
				goto load
			}
		}

		stack = stack.Next
		if stack == nil {
			if !valid {
				goto load
			}
			break
		}
	}

apply:
	if len(patchable) > 0 || len(modcache) > 0 {
		if err := apply(patchable, cfg); err != nil {
			fmt.Println("Unable to apply patches:", err)
			return err
		}
	}

	return nil
}

// Run the build + port process on a package
func port(pkg *packages.Package, cfg *Config) error {
	err := pkg.LoadSyntax()
	if err != nil {
		return err
	}

	// If this is the first time checking this package verify
	// that it has errors before we begin our investigation
	imports := make(map[string]bool, 0)
	illList := make([]error, 0)
	needTag := pkg.ExtFlags == stateBrokeParent
	if pkg.ExtFlags == stateUnknown {
		hasErr := false
		pkg.Build(nil, func(err packages.TypeError) {
			if iname, ok := err.Reason.(packages.TCBadImportName); ok {
				fname := err.Err.Fset.Position(err.Err.Pos).Filename
				imports[pkg.FileImports[fname][iname.PkgName]] = true
			} else if _, ok := err.Reason.(packages.TCBadName); ok {
				needTag = true
			} else {
				illList = append(illList, err)
			}
			if !hasErr {
				fmt.Println("Build errors occurred in:", pkg.ImportPath)
				hasErr = true
			}
			if cfg.Verbose {
				fmt.Println("\t" + err.Err.Error())
			}
		})

		// Never try porting a package with unknown type errors
		if len(illList) > 0 {
			err = fmt.Errorf("unknown type error(s) occurred in %v: %v", pkg.ImportPath, illList)
			return err
		}

		// If we saw no errors, move on
		if !needTag && len(imports) == 0 {
			pkg.ExtFlags = stateValid
			return nil
		}
	}

	// First lock the version the module will use (using the following process):
	//
	// 1. If the module can be updated we try locking it to the updated version
	// 2. If we already tried the updated version then lock it to the original version determined by MVS
	if mptc := modcache[pkg.Module.Path]; !pkg.Module.Main && ((mptc == nil && pkg.Module.Replace == nil) || (mptc != nil && mptc.action < modLocked)) {
		// Entering this point we should only have the following cases:
		// - Modules in the mod cache that haven't been updated or locked
		// - golang.org/x/... modules that haven't been updated
		mod := pkg.Module
		version := mod.Version
		// Used to track if we change the version we loaded
		updated := false

		if mptc == nil {
			mptc = &modulepatch{
				version: mod.Version,
				action:  modLocked,
			}
			modcache[pkg.Module.Path] = mptc

			uver, err := util.GoListModUpdate(mod.Path)
			if err != nil && !packages.IsExcludeGoListError(err.Error()){
				return err
			}

			updated := version != uver

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

		err = util.GoWorkEditReplaceVersion(
			mod.Path,
			version,
		)
		if err != nil {
			return err
		}

		forceLoad = updated

		// Exhaust golang.org/x/... packages
		if isGolangX(pkg.Module) {
			pkg.ExtFlags = stateExhausted
			return nil
		}

		if updated {
			pkg.ExtFlags = stateUnknown
			return nil
		}
	}

	// Have to do tagging
	if needTag {
		// Use the error filter to rebuild the import list whenever we decide to use a new config
		errIdx := 1
		pkg.CfgIdx = errIdx
		filterConfigs(pkg, func(err2 packages.TypeError) bool {
			if errIdx < pkg.CfgIdx {
				imports = make(map[string]bool)
			}

			if iname, ok := err2.Reason.(packages.TCBadImportName); ok && imports != nil {
				fname := err2.Err.Fset.Position(err2.Err.Pos).Filename
				imports[pkg.FileImports[fname][iname.PkgName]] = true
				return true
			}

			if !err2.Err.Soft && imports != nil {
				imports = nil
				return false
			}

			return err2.Err.Soft
		})

		if pkg.CfgIdx >= len(pkg.Configs) {
			return fmt.Errorf("unable to find a valid config")
		}

		if len(imports) == 0 {
			pkg.ExtFlags = statePatched
			// TODO: make patch
			return nil
		}

	}

	// We have reached the port dependencies stage

	// If we have have come back here after doing a run through the dependencies we should recheck the build
	if pkg.ExtFlags == statePortingDependencies {
		pkg.Build(nil, func(err packages.TypeError) {
			if iname, ok := err.Reason.(packages.TCBadImportName); ok {
				fname := err.Err.Fset.Position(err.Err.Pos).Filename
				imports[pkg.FileImports[fname][iname.PkgName]] = true
			} else {
				panic("unsanitized config used for doing porting of depenendencies")
			}
		})
	} else if len(imports) == 0 {
		panic("advancing to dependency porting but no bad dependencies found")
	}

	// If we fixed all the dependencies we can say this ported
	if len(imports) == 0 {
		if pkg.CfgIdx == 0 {
			pkg.ExtFlags = stateValid
		} else {
			pkg.ExtFlags = statePatched
		}
		// make patch
		return nil
	}

	// Keep track if any imports are portable
	canPortImports := false

	for path := range imports {
		ipkg := pkg.Imports[path]
		initProcFlags(ipkg)

		if ipkg.ExtFlags < stateExhausted {
			canPortImports = true
			if ipkg.ExtFlags < stateBrokeParent {
				ipkg.ExtFlags = stateBrokeParent
			}
		} else if ipkg.ExtFlags == statePatched {
			panic("package is claimed to be patchable but has bad parent")
		}
	}

	// Try porting imports first if possible
	if canPortImports {
		pkg.ExtFlags = statePortingDependencies
		return nil
	}

	// Try retagging to remove the bad imports
	fiEdits := make(fileImportEdits)
	expIdx := pkg.CfgIdx
	filterConfigs(pkg, func(te packages.TypeError) bool {
		if expIdx < pkg.CfgIdx && fiEdits == nil {
			expIdx = pkg.CfgIdx
			fiEdits = make(fileImportEdits)
		}

		if iname, ok := te.Reason.(packages.TCBadImportName); ok && fiEdits != nil {
			// If we haven't already found a build we can fix by changing imports
			// check all the errors on the current build to see if they could be changed
			fname := te.Err.Fset.Position(te.Err.Pos).Filename
			ipath := pkg.FileImports[fname][iname.PkgName]
			directives := cfg.Directives[ipath]
			if directives != nil && directives.Exports != nil {
				ed, ok := directives.Exports[iname.Name.Name]
				if ok {
					if fiEdits[fname] == nil {
						fiEdits[fname] = make(map[string]map[string]direct.ExportDirective)
						fiEdits[fname][iname.PkgName] = make(map[string]direct.ExportDirective)
					} else if fiEdits[fname][iname.PkgName] == nil {
						fiEdits[fname][iname.PkgName] = make(map[string]direct.ExportDirective)
					}

					fiEdits[fname][iname.PkgName][iname.Name.Name] = ed
					return false
				}
			}
		}

		if !te.Err.Soft && expIdx == pkg.CfgIdx {
			// Unreplaceable error, therefore disregard this config
			fiEdits = nil
			return false
		}

		return te.Err.Soft
	})

	if pkg.CfgIdx < len(pkg.Configs) {
		pkg.ExtFlags = statePatched
		return nil
	}

	pkgCacheDir := filepath.Join(cfg.Cache, pkg.ImportPath)
	err = os.MkdirAll(pkgCacheDir, 0740)
	if err != nil {
		return fmt.Errorf("unable to create cache directory for package: %w", err)
	}

	// Didn't find a working config, therefore we try to use export directives
	if fiEdits != nil {
		pkg.CfgIdx = expIdx
		err := applyExportDirective(pkg, pkgCacheDir, fiEdits)
		if err == nil {
			pkg.ExtFlags = statePatched
			return nil
		}
	}

	// We couldn't find a working config, so we try and see if we can fix the package using explicit handlers
	//
	if handler := cfg.Directives[pkg.ImportPath]; handler != nil && handler.Files != nil {
		err := applyPackageDirective(pkg, pkgCacheDir, handler.Files)
		// TODO: report on diff failing
		if err == nil {
			pkg.ExtFlags = statePatched
			return nil
		}
	}

	pkg.ExtFlags = stateExhausted
	return fmt.Errorf("no applicable options available to port package %v", pkg.ImportPath)
}

func apply(pkgs []*packages.Package, cfg *Config) error {
	showActions := cfg.Verbose || cfg.DryRun

	for path, ptc := range modcache {
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

	for _, pkg := range pkgs {
		fmt.Println("#", pkg.ImportPath)
		// Quick fix because we would lose the syntax on import change
		if err := pkg.LoadSyntax(); err != nil {
			return err
		}
		if pkg.CfgIdx == 0 {
			panic("trying to patch using default config (no changes)")
		}

		// Perform a clone to workspace if necessary
		if !pkg.Module.Main {
			path := filepath.Join(cfg.ImportDir, pkg.Module.Path)

			if cfg.UseVCS {
				if err := util.CloneModuleFromVCS(
					path,
					pkg.Module.Path,
					strings.TrimSuffix(pkg.Module.Version, "+incompatible"),
					); err != nil {
					return err
				}
			} else {
				if err := util.CloneModuleFromCache(pkg.Module.Dir, path, pkg.Module.Path); err != nil {
					return err
				}
			}

			if err := util.GoWorkEditDropReplace(pkg.Module.Path); err != nil {
				return err
			}

			// TODO: Go work use fails silently on a missing go.mod file, rerun 'go list' to verify it is now a main module position has changed
			if err := util.GoWorkUse(path); err != nil {
				return err
			}

			err := util.GoListModMain(pkg.Module.Path)
			if err != nil && !packages.IsExcludeGoListError(err.Error()) {
				return err
			}
			pkg.Module.Main = true
		}

		// TODO: find a better way to get the package directory
		dir, err := util.GoListPkgDir(pkg.ImportPath)
		if err != nil {
			if packages.IsExcludeGoListError(err.Error()) {
				dir = filepath.Join(cfg.ImportDir, pkg.Module.Path)
			} else {
				return err
			}
		}

		dcfg := pkg.Configs[0]
		pcfg := pkg.Configs[pkg.CfgIdx]

		// Print out description of patch
		// Platform == 0 (default) when we apply a manual patch
		if len(pcfg.Platforms) == 0 {
			fmt.Println("Applied manual patch")

			for _, gofile := range pcfg.GoFiles {
				if ovr := pcfg.Override[gofile.Name]; ovr != nil {
					util.CopyFile(filepath.Join(dir, gofile.Name), ovr.Path)
				}
			}
		} else {
			fmt.Print("Applying tags to match platform(s): ")
			for pidx, pltf := range pcfg.Platforms {
				fmt.Print(pltf)
				if pidx < len(pcfg.Platforms)-1 {
					fmt.Print(", ")
				}
			}
			fmt.Println()

			// Mark the files that were active in the default config
			current := make(map[*packages.GoFile]bool)
			for _, gofile := range dcfg.GoFiles {
				current[gofile] = true
			}

			// Apply changes to files that were changed
			for _, gofile := range pcfg.GoFiles {
				ovr := pcfg.Override[gofile.Name]
				if ovr != nil {
					// Copy overriden files from the cache
					newName := fmt.Sprintf("%v_%v.go", strings.TrimSuffix(gofile.Name, ".go"), packages.Goos)

					if showActions {
						fmt.Printf("%v: copied to %v\n", gofile.Name, newName)
					}

					if current[gofile] {
						if showActions {
							fmt.Printf("%v: added tag '!%v'\n", gofile.Name, packages.Goos)
						}

						if !cfg.DryRun {
							err := util.CopyFile(filepath.Join(dir, newName), ovr.Path)
							if err != nil {
								return err
							}

							src, err := util.Format(gofile.Syntax, pkg.Fset)
							if err != nil {
								return err
							}

							src, err = util.AppendTagString(src, "!"+packages.Goos, "&&", fmt.Sprintf(_TAG_NOTICE, "!"+packages.Goos))
							if err != nil {
								return err
							}

							err = os.WriteFile(filepath.Join(dir, gofile.Name), src, 0744)
							if err != nil {
								return err
							}
						}
					}

					if !cfg.DryRun {
						src, err := util.Format(ovr.Syntax, pkg.Fset)
						if err != nil {
							return err
						}

						src, err = util.AppendTagString(src, packages.Goos, "", fmt.Sprintf(_FILE_NOTICE, gofile.Name))
						if err != nil {
							return err
						}

						err = os.WriteFile(filepath.Join(dir, newName), src, 0744)
						if err != nil {
							return err
						}
					}

					// Describe the change changes
					if showActions {
						fmt.Printf("%v: added tag '%v'\n", newName, packages.Goos)
						for iname, symbols := range ovr.Meta.(map[string]map[string]direct.ExportDirective) {
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
								fmt.Printf("%v: replaced %v.%v with %v\n", newName, iname, symname, repstr)
							}
						}
					}

					if !showActions {
						fmt.Printf("%v: fixed imports\n", gofile.Name)
					}

				} else if !current[gofile] {
					// Add tags to files that were not in the default config
					fmt.Printf("%v: added %v tag\n", gofile.Name, packages.Goos)

					// the default config setting does not contain any files
					// this means "build constraints exclude all Go files" (_BUILD_CONSTRAINS_EXCLUDE_ALL_FILE)
					if len(dcfg.GoFiles) == 0 {
						newName := fmt.Sprintf("%v_%v.go", strings.TrimSuffix(gofile.Name, ".go"), packages.Goos)
						
						if showActions {
							fmt.Printf("%v: copied to %v\n", gofile.Name, newName)
						}

						err := util.CopyFile(filepath.Join(dir, newName), filepath.Join(dir, gofile.Name))
						if err != nil {
							return err
						}
					}

					src, err := util.Format(gofile.Syntax, pkg.Fset)
					if err != nil {
						return err
					}

					src, err = util.AppendTagString(src, packages.Goos, "||", fmt.Sprintf(_TAG_NOTICE, packages.Goos))
					if err != nil {
						return err
					}

					name := gofile.Name
					cnstr, _ := tags.ParseFileName(name)
					if cnstr != nil {
						name = strings.TrimSuffix(name, ".go") + "_" + packages.Goos + ".go"
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
				fmt.Printf("%v: added !%v tag\n", gofile.Name, packages.Goos)

				src, err := util.Format(gofile.Syntax, pkg.Fset)
				if err != nil {
					return err
				}

				src, err = util.AppendTagString(src, "!"+packages.Goos, "&&", fmt.Sprintf(_TAG_NOTICE, "!"+packages.Goos))
				if err != nil {
					return err
				}

				err = os.WriteFile(filepath.Join(dir, gofile.Name), src, 0744)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func typeCheck(pkg *packages.Package, filter func(packages.TypeError) bool) bool {
	// Step 1: Build the package we are testing first
	passed := true

	handleErr := func(err packages.TypeError) {
		// Filter import errors if set to
		doPass := err.Err.Soft
		if filter != nil {
			doPass = filter(err)
		}
		passed = passed && doPass
	}

	typed, _ := pkg.Build(nil, handleErr)

	// We hit an error therefore we need to stop
	if !passed {
		return false
	}

	pkg.Types = typed

	// Step 2: Build the parent packages using this version of the package

	// FileName -> PkgName

	for _, parent := range pkg.Parents {
		var inames map[string]*string
		pcfg := parent.Configs[parent.CfgIdx]

		importer := packages.Importer(func(path string) (*types.Package, error) {
			if path == pkg.ImportPath {
				return typed, nil
			}
			return nil, nil
		})

		handleErr := func(err packages.TypeError) {
			// We only care about errors from local imports
			if info, ok := err.Reason.(packages.TCBadImportName); ok {
				// Grab our cached import names
				if inames == nil {
					// Make the cache if it doesn't exist
					inames = make(map[string]*string, len(pcfg.GoFiles))
					for idx := range pcfg.GoFiles {
						inames[pcfg.GoFiles[idx].Name] = packages.FindImportName(pcfg.Syntax[idx], pkg.ImportPath)
					}
				}
				iname := inames[err.Err.Fset.Position(err.Err.Pos).Filename]

				// If we have a match then that means the parents failed because of
				// of the package under test, therefore we have a bad build
				passed = passed && iname != nil && info.PkgName != *iname
			} else if !err.Err.Soft {
				// TODO: handle gracefully (allow cleanup)
				// panic(
				// 	fmt.Sprintf(
				// 		"unsanitized parent package: %v for %v has error %v",
				// 		parent.ImportPath, pkg.ImportPath,
				// 		err.Err.Error(),
				// 	),
				// )

			}
		}

		parent.Build(importer, handleErr)
	}

	return passed
}

// Package is an golang.org/x/... package
func isGolangX(mod *packages.Module) bool {
	return mod != nil && strings.HasPrefix(mod.Path, "golang.org/x/")
}
