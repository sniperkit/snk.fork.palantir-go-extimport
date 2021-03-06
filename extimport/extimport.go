/*
Sniperkit-Bot
- Status: analyzed
*/

// Copyright 2016 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package extimport

import (
	"fmt"
	"go/build"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pkg/errors"
)

func Run(projectDir string, pkgs []string, list, all bool, w io.Writer) error {
	wd, err := os.Getwd()
	if err != nil {
		return errors.Wrapf(err, "failed to determine working directory")
	}

	// projectDir must be an absolute path to ensure that build.Import* calls populate import paths properly
	if !filepath.IsAbs(projectDir) {
		projectDir = path.Join(wd, projectDir)
	}

	projectDirPkg, _ := build.ImportDir(projectDir, build.FindOnly)
	projectDirImportPath := projectDirPkg.ImportPath

	internalPkgs := make(map[string]bool)
	externalPkgs := make(map[string][]string)
	printedPkgs := make(map[string]bool)

	type pkgWithSrc struct {
		pkgPath string
		srcDir  string
	}

	externalImportsExist := false
	pkgsToProcess := make([]pkgWithSrc, 0, len(pkgs))
	for _, pkgPath := range pkgs {
		// skip testdata packages
		if strings.Contains(pkgPath, "/testdata/") || strings.HasSuffix(pkgPath, "/testdata") {
			continue
		}
		srcDir := path.Join(wd, pkgPath)
		srcDirPkg, _ := build.ImportDir(srcDir, build.ImportComment)
		if !strings.HasPrefix(srcDirPkg.ImportPath, projectDirImportPath) {
			return errors.Errorf("package %s is not within project directory %q: import path %s is not within %s", srcDir, projectDir, srcDirPkg.ImportPath, projectDirImportPath)
		}
		pkgsToProcess = append(pkgsToProcess, pkgWithSrc{
			pkgPath: ".",
			srcDir:  srcDir,
		})
	}
	processedPkgs := make(map[pkgWithSrc]bool)
	for len(pkgsToProcess) > 0 {
		currPkg := pkgsToProcess[0]
		pkgsToProcess = pkgsToProcess[1:]
		if processedPkgs[currPkg] {
			continue
		}
		processedPkgs[currPkg] = true

		externalPkgs, err := checkImports(currPkg.pkgPath, currPkg.srcDir, projectDir, wd, internalPkgs, externalPkgs, printedPkgs, list, w)
		if err != nil {
			return errors.Wrapf(err, "failed to check imports for %s", currPkg)
		} else if len(externalPkgs) == 0 {
			continue
		}

		externalImportsExist = true
		if list && all {
			// when run in "list all" mode, process all external packages as well so that all
			// external dependencies (even those multiple levels deep) are listed
			for _, currExternalPkg := range externalPkgs {
				externalPkgWithSrc := pkgWithSrc{
					pkgPath: currExternalPkg,
					srcDir:  currPkg.srcDir,
				}
				if !processedPkgs[externalPkgWithSrc] {
					pkgsToProcess = append(pkgsToProcess, externalPkgWithSrc)
				}
			}
		}
	}

	if externalImportsExist {
		return fmt.Errorf("")
	}
	return nil
}

// checkImports returns any external imports for the package "pkgPath". Does so by getting the "import" statements in
// all of the .go files (including tests) in the directory and then resolving the imports using standard Go rules
// assuming that the resolution occurs in "srcDir" (this is done so that special directories like "vendor" and
// "internal" are handled correctly). An import is considered external if its resolved location is outside of the
// directory tree of "projectRootDir".
func checkImports(pkgPath, srcDir, projectRootDir, wd string, internalPkgs map[string]bool, externalPkgs map[string][]string, printedPkgs map[string]bool, list bool, w io.Writer) ([]string, error) {
	// get all imports in package
	pkg, err := build.Import(pkgPath, srcDir, build.ImportComment)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to import package %s using srcDir %s", pkgPath, srcDir)
	}
	importsToCheck := make(map[string][]token.Position)
	addImportPosToMap(importsToCheck, pkg.ImportPos)
	addImportPosToMap(importsToCheck, pkg.TestImportPos)
	addImportPosToMap(importsToCheck, pkg.XTestImportPos)

	var externalPkgsFound []string
	// check imports for each file in the package
	sortedFiles, fileToImports := fileToImportsMap(importsToCheck)
	for _, currFile := range sortedFiles {
		// check each import in the file
		for _, currImportLine := range fileToImports[currFile] {
			chain, err := getExternalImport(currImportLine.name, srcDir, projectRootDir, internalPkgs, externalPkgs)
			if err != nil {
				return nil, errors.Wrapf(err, "isExternalImport failed for %s", currImportLine)
			}

			if len(chain) > 0 {
				externalPkg := chain[len(chain)-1]
				externalPkgsFound = append(externalPkgsFound, externalPkg)
				if list {
					if _, ok := printedPkgs[externalPkg]; !ok {
						fmt.Fprintln(w, externalPkg)
					}
					printedPkgs[externalPkg] = true
				} else {
					msg := fmt.Sprintf("%s:%d:%d: imports external package %s", currFile, currImportLine.pos.Line, currImportLine.pos.Column, externalPkg)
					if len(chain) > 1 {
						msg += fmt.Sprintf(" transitively via %s", strings.Join(chain[:len(chain)-1], " -> "))
					}
					fmt.Fprintln(w, msg)
				}
			}
		}
	}
	return externalPkgsFound, nil
}

// getExternalImport takes an import and returns the chain to the external import if the import is external and nil
// otherwise. Assumes that the import occurs in a package in "srcDir". The import is considered external if its resolved
// path is not a subdirectory of the project root.
func getExternalImport(importPkgPath, srcDir, projectRoot string, internalPkgs map[string]bool, externalPkgs map[string][]string) ([]string, error) {
	if !strings.Contains(importPkgPath, ".") || internalPkgs[importPkgPath] {
		// if package is a standard package or known to be internal, return empty
		return nil, nil
	} else if chain, ok := externalPkgs[importPkgPath]; ok {
		// if package is external and result is cached, return directly
		return chain, nil
	}

	pkg, err := build.Import(importPkgPath, srcDir, build.ImportComment)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to import package %s", importPkgPath)
	}

	// import is external if it is not a standard go package and is not a subdirectory of the project root
	if rel, err := filepath.Rel(projectRoot, pkg.Dir); err != nil || strings.HasPrefix(rel, "../") {
		currChain := []string{importPkgPath}
		externalPkgs[importPkgPath] = currChain
		return currChain, nil
	}

	// current import is internal, but check if any of its imports are external. Resolve the imports for this
	// imported package using its source directory (required because this import may have its own internal or vendor
	// directories).
	sort.Strings(pkg.Imports)
	for _, currImport := range pkg.Imports {
		chain, err := getExternalImport(currImport, pkg.Dir, projectRoot, internalPkgs, externalPkgs)
		if err != nil {
			return nil, errors.Wrapf(err, "isExternalImport failed for %s", currImport)
		}
		// if any import is external, this import is external
		if len(chain) > 0 {
			currChain := append([]string{importPkgPath}, chain...)
			externalPkgs[importPkgPath] = currChain
			return currChain, nil
		}
	}

	// if all checks pass, mark this package as internal and return false
	internalPkgs[importPkgPath] = true
	return nil, nil
}

func addImportPosToMap(dst, src map[string][]token.Position) {
	for k, v := range src {
		dst[k] = v
	}
}

type importLine struct {
	name string
	pos  token.Position
}

type byLineNum []importLine

func (a byLineNum) Len() int      { return len(a) }
func (a byLineNum) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a byLineNum) Less(i, j int) bool {
	if a[i].pos.Line == a[j].pos.Line {
		// if line numbers are the same, do secondary sort by column position
		return a[i].pos.Column < a[j].pos.Column
	}
	return a[i].pos.Line < a[j].pos.Line
}

func fileToImportsMap(importPos map[string][]token.Position) ([]string, map[string][]importLine) {
	output := make(map[string][]importLine)
	for k, v := range importPos {
		for _, currPos := range v {
			output[currPos.Filename] = append(output[currPos.Filename], importLine{
				name: k,
				pos:  currPos,
			})
		}
	}

	var sortedKeys []string
	for k, v := range output {
		sortedKeys = append(sortedKeys, k)
		sort.Sort(byLineNum(v))
	}
	sort.Strings(sortedKeys)
	return sortedKeys, output
}
