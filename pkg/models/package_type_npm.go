package models

const PackageNPM PackageType = "npm"

func init() { registerPackageType(PackageNPM) }
