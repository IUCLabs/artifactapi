package models

import "fmt"

type PackageType string

var validPackageTypes = map[PackageType]bool{}

func registerPackageType(p PackageType) {
	validPackageTypes[p] = true
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
