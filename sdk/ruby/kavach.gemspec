require_relative "lib/kavach/version"

Gem::Specification.new do |s|
  s.name = "kavach"
  s.version = Kavach::VERSION
  s.summary = "Kavach SDK for Ruby: record what a handler did before it failed, replay it to verify a fix"
  s.authors = ["Kavach Labs"]
  s.license = "Apache-2.0"
  s.required_ruby_version = ">= 3.1"
  s.files = Dir["lib/**/*.rb", "README.md"]
  s.require_paths = ["lib"]
end
