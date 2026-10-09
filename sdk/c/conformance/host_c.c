/* kavach-conformance-host-c: the conformance handler (SPEC 9.6) as a host. */
#include <stdio.h>

#include "conformance.h"

int main(int argc, char** argv) {
  kavach_host_options opts = {0};
  opts.sdk = "kavach-c/" KAVACH_VERSION;
  kavach_maybe_host(argc, argv, conf_factory, &opts);
  fprintf(stderr, "usage: %s kavach-host\n", argv[0]);
  return 2;
}
