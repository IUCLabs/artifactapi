package models

import "fmt"

type PackageType string

const (
	PackageGeneric   PackageType = "generic"
	PackageDocker    PackageType = "docker"
	PackageHelm      PackageType = "helm"
	PackagePyPI      PackageType = "pypi"
	PackageNPM       PackageType = "npm"
	PackageRPM       PackageType = "rpm"
	PackageAlpine    PackageType = "alpine"
	PackagePuppet    PackageType = "puppet"
	PackageTerraform PackageType = "terraform"
	PackageGoProxy   PackageType = "goproxy"
	PackageGitHubRPM PackageType = "github_rpm"
)

var validPackageTypes = map[PackageType]bool{
	PackageGeneric:   true,
	PackageDocker:    true,
	PackageHelm:      true,
	PackagePyPI:      true,
	PackageNPM:       true,
	PackageRPM:       true,
	PackageAlpine:    true,
	PackagePuppet:    true,
	PackageTerraform: true,
	PackageGoProxy:   true,
	PackageGitHubRPM: true,
}

func (p PackageType) Valid() bool {
	return validPackageTypes[p]
}

func (p PackageType) String() string {
	return string(p)
}

func ParsePackageType(s string) (PackageType, error) {
	pt := PackageType(s)
	if !pt.Valid() {
		return "", fmt.Errorf("unknown package type: %q", s)
	}
	return pt, nil
}
