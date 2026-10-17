# `gems` virtual

A gems virtual serves one RubyGems source over local and remote gems members:
`gem install` and `bundle install` work against `/api/v1/virtual/<name>/`.

- `info/<gem>` is the union of member lines; on a duplicate version-platform
  the earlier member wins. A gem only one member has is served verbatim.
- `versions` regenerates the line (and md5) of every gem several members list
  from its merged `info`; other gems' lines pass through. `names` is the union.
- `gems/*.gem` and `quick/Marshal.4.8/*.gemspec.rz` 302 to the member whose
  `info` line won.
- A member error fails the request with 502; a member without the gem is
  skipped. Merges are cached for 60s.

```hcl
resource "artifactapi_virtual" "gems" {
  name         = "gems"
  package_type = "gems"
  members      = [artifactapi_local_gems.internal.name, artifactapi_remote_gems.rubygems.name]
}
```

```ruby
# Gemfile
source "https://artifactapi.example.com/api/v1/virtual/gems/"
```

Push to the local member directly; the virtual is read-only.
