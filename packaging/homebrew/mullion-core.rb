# Homebrew-core candidate formula: builds Mullion from source, as core
# requires (the tap formula ships prebuilt binaries instead).
#
# Submit it once the project meets homebrew-core's notability bar
# (roughly 75+ GitHub stars) by opening a PR against Homebrew/homebrew-core
# adding this file as Formula/m/mullion.rb — update `url` to the latest
# tag and fill in `sha256` first:
#   curl -fsSL <url> | shasum -a 256
# Then check it locally with:
#   brew install --build-from-source ./mullion-core.rb
#   brew audit --new --formula ./mullion-core.rb
class Mullion < Formula
  desc "PHP version manager and local dev server with Caddy, MySQL, and HTTPS"
  homepage "https://github.com/anabiiil/mullion"
  url "https://github.com/anabiiil/mullion/archive/refs/tags/v1.2.0.tar.gz"
  sha256 "FILL_ME_IN_FROM_THE_TAGGED_TARBALL"
  license "MIT"
  head "https://github.com/anabiiil/mullion.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w")
  end

  def caveats
    <<~EOS
      Set up the full stack (Caddy, latest PHP, Composer, MySQL, phpMyAdmin) with:
        mullion setup
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/mullion --version")
  end
end
