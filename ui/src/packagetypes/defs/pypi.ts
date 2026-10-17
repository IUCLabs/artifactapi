import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'blue',
  snippets: ({ proxy }) => {
    return [
      {
        title: 'Install a package (one-off)',
        language: 'bash',
        code: `pip install --index-url ${proxy}/simple/ <package>`,
      },
      {
        title: 'Configure pip persistently',
        language: 'bash',
        code: `mkdir -p ~/.config/pip
cat > ~/.config/pip/pip.conf <<'EOF'
[global]
index-url = ${proxy}/simple/
EOF

pip install <package>`,
      },
    ];
  },
};

export default def;
