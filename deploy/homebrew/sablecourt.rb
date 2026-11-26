# Homebrew formula template for github.com/shadewright-labs/homebrew-tap
# Install after publishing a GitHub release:
#   brew install shadewright-labs/tap/sablecourt
class Sablecourt < Formula
  desc "Active orchestrator for heterogeneous AI and non-LLM workloads"
  homepage "https://github.com/shadewright-labs/sablecourt"
  version "0.1.0"
  license "Apache-2.0"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/shadewright-labs/sablecourt/releases/download/v#{version}/sablecourt_#{version}_darwin_arm64.tar.gz"
      sha256 "REPLACE_WITH_DARWIN_ARM64_SHA256"
    else
      url "https://github.com/shadewright-labs/sablecourt/releases/download/v#{version}/sablecourt_#{version}_darwin_amd64.tar.gz"
      sha256 "REPLACE_WITH_DARWIN_AMD64_SHA256"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/shadewright-labs/sablecourt/releases/download/v#{version}/sablecourt_#{version}_linux_arm64.tar.gz"
      sha256 "REPLACE_WITH_LINUX_ARM64_SHA256"
    else
      url "https://github.com/shadewright-labs/sablecourt/releases/download/v#{version}/sablecourt_#{version}_linux_amd64.tar.gz"
      sha256 "REPLACE_WITH_LINUX_AMD64_SHA256"
    end
  end

  def install
    bin.install "sablecourtd"
    bin.install "sablectl"
  end

  test do
    assert_match "sablectl", shell_output("#{bin}/sablectl version")
  end
end
