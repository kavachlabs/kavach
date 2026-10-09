// kavach-conformance-host-cpp: the conformance handler (SPEC 9.6) as a host.
#include <cstdio>
#include <memory>

#include "conformance.hpp"

int main(int argc, char** argv) {
  kavach::maybe_host(argc, argv, [] { return std::make_unique<ConfHandler>(); });
  std::fprintf(stderr, "usage: %s kavach-host\n", argv[0]);
  return 2;
}
