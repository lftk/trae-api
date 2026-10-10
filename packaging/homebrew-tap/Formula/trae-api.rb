class TraeApi < Formula
  desc "OpenAI-compatible stateless Chat Completions gateway for Trae CN"
  homepage "https://github.com/lftk/trae-api"
  url "https://github.com/lftk/trae-api/archive/refs/tags/v0.1.0.tar.gz"
  version "0.1.0"
  sha256 "REPLACE_WITH_RELEASE_TARBALL_SHA256" # sha256 of the v0.1.0 tarball, set at release time
  license "Apache-2.0"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=v#{version}")
  end

  service do
    run [opt_bin/"trae-api"]
    keep_alive true
    log_path var/"log/trae-api.log"
    error_log_path var/"log/trae-api.log"
  end

  test do
    assert_match "trae-api v#{version}", shell_output("#{bin}/trae-api --version")
    assert_match "TRAE_API_ADDR", shell_output("#{bin}/trae-api --help")
  end
end
