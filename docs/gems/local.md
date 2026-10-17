# Local gems repository

A local `gems` repo is a RubyGems source: `gem push`, `gem yank`, `gem install`
and `bundle install` work against `/api/v1/local/<repo>`. `gem push` needs an
API key; any value works.

```bash
GEM_HOST_API_KEY=unused gem push --host https://artifactapi.example.com/api/v1/local/<repo> mygem-1.0.0.gem
gem install mygem --source https://artifactapi.example.com/api/v1/local/<repo>/
gem yank mygem -v 1.0.0 --host https://artifactapi.example.com/api/v1/local/<repo>
```

```ruby
# Gemfile
source "https://artifactapi.example.com/api/v1/local/<repo>/"
gem "mygem"
```

The repo serves the compact index (`versions`, `info/<gem>`, `names`), `gems/*.gem`
and `quick/Marshal.4.8/*.gemspec.rz`. The legacy `specs.4.8.gz` indexes are not
served; current gem and bundler clients use the compact index.

Versions are immutable: pushing an existing name-version-platform is rejected
with 409. A yanked version disappears from the index and its files are removed.
