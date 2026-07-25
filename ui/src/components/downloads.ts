// Per-repo-type "downloadable" capability map.
//
// When a repo's package_type has an entry here, file entries in the object
// browser render as direct-download links pointing at the URL the entry
// builds. Types absent from the map render as plain text. Enabling a new
// type is a one-line addition below.
//
// Download routes are same-origin, unauthenticated GETs (the API serves local
// repos as real registries with no token on reads), so a bare <a href download>
// works and carries no credentials.

// buildUrl receives the repo name and the artifact's full path (may contain
// slashes) and returns the direct-download URL for that file.
type DownloadUrlBuilder = (repo: string, path: string) => string;

export const downloadableTypes: Record<string, DownloadUrlBuilder> = {
  // rpm locals are real yum repos; files are served at
  // /api/v2/remotes/<repo>/files/<path>.
  rpm: (repo, path) =>
    `/api/v2/remotes/${encodeURIComponent(repo)}/files/${path
      .split('/')
      .map(encodeURIComponent)
      .join('/')}`,
};

// downloadUrlFor returns the direct-download URL for a file when its repo type
// is downloadable, or null otherwise (render as plain text).
export function downloadUrlFor(
  packageType: string | undefined,
  repo: string,
  path: string,
): string | null {
  if (!packageType) return null;
  const build = downloadableTypes[packageType];
  return build ? build(repo, path) : null;
}
