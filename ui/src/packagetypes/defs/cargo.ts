import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'yellow',
  snippets: ({ name, proxy }) => {
    return [
      {
        title: 'Point crates.io at this sparse registry',
        language: 'toml',
        code: `# ~/.cargo/config.toml
[source.crates-io]
replace-with = "${name}"

[registries.${name}]
index = "sparse+${proxy}/"`,
        note: 'config.json is served by artifactapi so crate downloads also go through this remote.',
      },
    ];
  },
};

export default def;
