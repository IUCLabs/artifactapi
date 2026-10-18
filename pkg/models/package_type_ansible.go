package models

const PackageAnsible PackageType = "ansible"

func init() { registerPackageType(PackageAnsible) }
