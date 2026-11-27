//go:build e2e

package e2e

import (
	"testing"

	"github.com/testcontainers/testcontainers-go"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	imgAlma      = "docker.io/library/almalinux:9.7@sha256:2d57aea965da8fbda97736ec5125c1b683ce271438038ea1906b2391e89b77d2"
	imgDebian    = "docker.io/library/debian:trixie-slim@sha256:8aa5a7bf5a1955bddbe7d74fcdd0b91ab30e74362a8b73af672b0481b35b44a0"
	imgAlpine    = "docker.io/library/alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2"
	imgPython    = "docker.io/library/python:3.13-slim@sha256:7249d5587b78209a006dc4a3b5340678f84ea729239193aa559962cd8c1fce50"
	imgUV        = "ghcr.io/astral-sh/uv:python3.13-trixie-slim@sha256:00e2b362982a5ce19eb3a51453ed76af92ccaaedccc0236fae83b27d994ae0f3"
	imgNode      = "docker.io/library/node:24-alpine@sha256:116b800c3754b440ee0aa3ed2f20f5bf3d99a554bfd9adeea09ea8017f6e85f5"
	imgDockerCLI = "docker.io/library/docker:29-cli@sha256:4e60c31615e83523afbb59773b25d055c0da9edcd5f80a4c6a6eebad49de1a25"
	imgGo        = "docker.io/library/golang:1.26.0-alpine@sha256:d4c4845f5d60c6a974c6000ce58ae079328d03ab7f721a0734277e69905473e5"
	imgHelm      = "docker.io/alpine/helm:3.19.0@sha256:aef9b56f64e866207d9591d0abd8f6d767b36aadd12edf68f8a719716d9d29c9"
	imgRust      = "docker.io/library/rust:1-slim@sha256:3b199fd6db7d709274df56565b8f4003f9e30d7bfcff9d53148c86053b7855e2"
	imgRuby      = "docker.io/library/ruby:3.4-slim@sha256:f038b6952ce49d4d7bcbe2a6c194299f5c70b45fc6d9f9fec4798c4c0f61aed3"
	imgDotnet    = "mcr.microsoft.com/dotnet/sdk:10.0@sha256:8c6edfb26c85153f236d26efebd9ad349aec403583f343f201ee67838b23658d"
	imgOpenVox   = "ghcr.io/openvoxproject/openvoxagent:8.31.0@sha256:f0120b984c86e8de2a6a005d54981332117dbe7ca41d87e44ebef3e7425c17a0"
	imgTerraform = "docker.io/hashicorp/terraform:1.14.0@sha256:3abcdb56739bf9c61a0290cfd1a2e41ef9c3799c0e6fa7f3c467f883367d3ecb"
	imgCurl      = "docker.io/curlimages/curl:8.17.0@sha256:935d9100e9ba842cdb060de42472c7ca90cfe9a7c96e4dacb55e79e560b3ff40"
)

func TestClients(t *testing.T) {
	cases := []struct {
		name string
		test func(t *testing.T)
	}{
		{"dnf", testDNF},
		{"apt", testAPT},
		{"apk", testAPK},
		{"pip", testPip},
		{"uv", testUV},
		{"npm", testNPM},
		{"docker", testDocker},
		{"go", testGo},
		{"helm", testHelm},
		{"cargo", testCargo},
		{"gem", testGem},
		{"dotnet", testDotnet},
		{"puppet", testPuppet},
		{"ansible-galaxy", testAnsibleGalaxy},
		{"terraform", testTerraform},
		{"curl-generic", testGeneric},
		{"npm-vcs", testVCS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.test(t)
		})
	}
}

func testDNF(t *testing.T) {
	r := withRemote(t, "cl-rpm", models.PackageRPM, "https://repo.almalinux.org/almalinux")
	missThenHit(t, clientRun{image: imgAlma, script: `
rm -f /etc/yum.repos.d/*.repo
cat > /etc/yum.repos.d/aa.repo <<EOF
[aa-baseos]
name=aa-baseos
baseurl=$AA/api/v1/remote/cl-rpm/9/BaseOS/\$basearch/os/
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-AlmaLinux-9
EOF
dnf -y --setopt=retries=5 install tree
tree --version
`}, cached{r, re(`/Packages/tree-[^/]+\.rpm$`)})
}

func testAPT(t *testing.T) {
	r := withRemote(t, "cl-deb", models.PackageDeb, "https://deb.debian.org/debian")
	missThenHit(t, clientRun{image: imgDebian, script: `
rm -f /etc/apt/sources.list.d/* /etc/apt/sources.list
cat > /etc/apt/sources.list.d/aa.sources <<EOF
Types: deb
URIs: $AA/api/v1/remote/cl-deb
Suites: trixie
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
EOF
apt-get -o Acquire::Retries=5 update
apt-get -o Acquire::Retries=5 install -y --no-install-recommends tree
tree --version
`}, cached{r, re(`^pool/main/t/tree/tree_[^/]+\.deb$`)})
}

func testAPK(t *testing.T) {
	r := withRemote(t, "cl-apk", models.PackageAlpine, "https://dl-cdn.alpinelinux.org/alpine")
	missThenHit(t, clientRun{image: imgAlpine, script: `
echo "$AA/api/v1/remote/cl-apk/v3.23/main" > /etc/apk/repositories
apk add --no-cache tree
tree --version
`}, cached{r, re(`^v3\.23/main/x86_64/tree-[^/]+\.apk$`)})
}

func testPip(t *testing.T) {
	r := withRemote(t, "cl-pypi", models.PackagePyPI, "https://files.pythonhosted.org")
	missThenHit(t, clientRun{image: imgPython, script: `
pip install --no-cache-dir --retries 5 --index-url "$AA/api/v1/remote/cl-pypi/simple/" --trusted-host "${AA_HOST%:*}" six==1.17.0
python -c 'import six; print(six.__version__)'
`}, cached{r, re(`/six-1\.17\.0-py2\.py3-none-any\.whl$`)})
}

func testUV(t *testing.T) {
	r := withRemote(t, "cl-uv", models.PackagePyPI, "https://files.pythonhosted.org")
	missThenHit(t, clientRun{image: imgUV, script: `
uv pip install --system --no-cache --index-url "$AA/api/v1/remote/cl-uv/simple/" --allow-insecure-host "${AA_HOST%:*}" idna==3.10
python -c 'import idna; print(idna.__version__)'
`}, cached{r, re(`/idna-3\.10-py3-none-any\.whl$`)})
}

func testNPM(t *testing.T) {
	r := withRemote(t, "cl-npm", models.PackageNPM, "https://registry.npmjs.org")
	missThenHit(t, clientRun{image: imgNode, script: `
mkdir /w && cd /w
npm install --no-audit --no-fund --fetch-retries=5 --registry "$AA/api/v1/remote/cl-npm/" is-number@7.0.0
node -e 'require("is-number")'
`}, cached{r, re(`^is-number/-/is-number-7\.0\.0\.tgz$`)})
}

func testDocker(t *testing.T) {
	r := withRemote(t, "cl-docker", models.PackageDocker, "https://registry-1.docker.io")
	sock := testcontainers.MustExtractDockerSocket(t.Context())
	missThenHit(t, clientRun{
		image: imgDockerCLI,
		binds: []string{sock + ":/var/run/docker.sock"},
		env:   map[string]string{"REF": "127.0.0.1:" + serverPort + "/cl-docker/library/busybox:1.36.1-musl"},
		script: `
docker image rm -f "$REF" >/dev/null 2>&1 || true
docker pull "$REF"
docker image rm -f "$REF"
`}, cached{r, re(`/blobs/sha256:[0-9a-f]{64}$`)})
}

func testGo(t *testing.T) {
	r := withRemote(t, "cl-go", models.PackageGoProxy, "https://proxy.golang.org")
	missThenHit(t, clientRun{image: imgGo, script: `
export GOPROXY="$AA/api/v1/remote/cl-go" GOFLAGS=-modcacherw GOMODCACHE=/tmp/modcache
go mod download -x github.com/google/uuid@v1.6.0
`}, cached{r, re(`^github\.com/google/uuid/@v/v1\.6\.0\.zip$`)})
}

func testHelm(t *testing.T) {
	r := withRemote(t, "cl-helm", models.PackageHelm, "https://charts.jetstack.io")
	missThenHit(t, clientRun{image: imgHelm, script: `
helm pull cert-manager --repo "$AA/api/v1/remote/cl-helm" --version v1.16.2 -d /tmp
ls /tmp/cert-manager-v1.16.2.tgz
`}, cached{r, re(`cert-manager-v1\.16\.2\.tgz$`)})
}

func testCargo(t *testing.T) {
	r := withRemote(t, "cl-cargo", models.PackageCargo, "https://index.crates.io")
	missThenHit(t, clientRun{image: imgRust, script: `
export CARGO_HOME=/tmp/cargo CARGO_NET_RETRY=5
mkdir -p $CARGO_HOME /w/src && cd /w
cat > $CARGO_HOME/config.toml <<EOF
[source.crates-io]
replace-with = "aa"
[source.aa]
registry = "sparse+$AA/api/v1/remote/cl-cargo/"
EOF
cat > Cargo.toml <<EOF
[package]
name = "e2e"
version = "0.1.0"
edition = "2021"
[dependencies]
itoa = "=1.0.11"
EOF
echo 'fn main() {}' > src/main.rs
cargo fetch
`}, cached{r, re(`^crates/itoa/itoa-1\.0\.11\.crate$`)})
}

func testGem(t *testing.T) {
	r := withRemote(t, "cl-gems", models.PackageGems, "https://rubygems.org")
	missThenHit(t, clientRun{image: imgRuby, script: `
gem install --clear-sources --source "$AA/api/v1/remote/cl-gems/" --no-document rack -v 3.1.8
ruby -e 'require "rack"; puts Rack.release'
`}, cached{r, re(`^gems/rack-3\.1\.8\.gem$`)})
}

func testDotnet(t *testing.T) {
	r := withRemote(t, "cl-nuget", models.PackageNuGet, "https://api.nuget.org/v3/index.json")
	missThenHit(t, clientRun{
		image: imgDotnet,
		tls:   true,
		env:   map[string]string{"DOTNET_CLI_TELEMETRY_OPTOUT": "1", "DOTNET_NOLOGO": "1", "NUGET_PACKAGES": "/tmp/nuget"},
		script: `
update-ca-certificates >/dev/null
mkdir /w && cd /w
cat > nuget.config <<EOF
<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="aa" value="$AA_TLS/api/v1/remote/cl-nuget/index.json" />
  </packageSources>
</configuration>
EOF
cat > e2e.csproj <<EOF
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup>
  <ItemGroup><PackageReference Include="Newtonsoft.Json" Version="13.0.3" /></ItemGroup>
</Project>
EOF
dotnet restore
`}, cached{r, re(`newtonsoft\.json\.13\.0\.3\.nupkg$`)})
}

func testPuppet(t *testing.T) {
	r := withRemote(t, "cl-forge", models.PackagePuppet, "https://forgeapi.puppet.com")
	missThenHit(t, clientRun{image: imgOpenVox, script: `
export PATH=/opt/puppetlabs/bin:$PATH
puppet module install puppetlabs-stdlib --version 9.7.0 --ignore-dependencies --target-dir /tmp/modules --module_repository "$AA/api/v1/remote/cl-forge"
ls /tmp/modules/stdlib/metadata.json
`}, cached{r, re(`puppetlabs-stdlib-9\.7\.0\.tar\.gz$`)})
}

func testAnsibleGalaxy(t *testing.T) {
	r := withRemote(t, "cl-galaxy", models.PackageAnsible, "https://galaxy.ansible.com")
	missThenHit(t, clientRun{image: imgPython, script: `
pip install --no-cache-dir --retries 5 -q ansible-core==2.19.4
ansible-galaxy collection install ansible.posix:==2.1.0 -p /tmp/collections -s "$AA/api/v1/remote/cl-galaxy/api/"
ls /tmp/collections/ansible_collections/ansible/posix/MANIFEST.json
`}, cached{r, re(`ansible-posix-2\.1\.0\.tar\.gz$`)})
}

func testTerraform(t *testing.T) {
	r := withRemote(t, "cl-tf", models.PackageTerraform, "https://registry.terraform.io")
	missThenHit(t, clientRun{image: imgTerraform, script: `
mkdir /w && cd /w
cat > /tmp/tf.rc <<EOF
host "registry.terraform.io" {
  services = {
    "providers.v1" = "$AA/api/v1/remote/cl-tf/"
  }
}
EOF
cat > main.tf <<EOF
terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "3.2.4"
    }
  }
}
EOF
TF_CLI_CONFIG_FILE=/tmp/tf.rc terraform init -input=false
`}, cached{r, re(`^hashicorp/null/3\.2\.4/download/linux/amd64$`)})
}

func testGeneric(t *testing.T) {
	r := withRemote(t, "cl-generic", models.PackageGeneric, "https://github.com")
	missThenHit(t, clientRun{image: imgCurl, script: `
curl -fsSL --retry 5 -o /tmp/yq "$AA/api/v1/remote/cl-generic/mikefarah/yq/releases/download/v4.44.3/yq_linux_amd64"
chmod +x /tmp/yq && /tmp/yq --version
`}, cached{r, re(`^mikefarah/yq/releases/download/v4\.44\.3/yq_linux_amd64$`)})
}

func testVCS(t *testing.T) {
	r := withRemote(t, "cl-vcs", models.PackageVCS, "https://github.com")
	missThenHit(t, clientRun{image: imgNode, script: `
mkdir /w && cd /w
npm install --no-audit --no-fund --fetch-retries=5 "$AA/api/v1/remote/cl-vcs/jonschlinkert/is-number/archive/refs/tags/7.0.0.tar.gz"
node -e 'require("is-number")'
`}, cached{r, re(`^jonschlinkert/is-number/archive/refs/tags/7\.0\.0\.tar\.gz$`)})
}
