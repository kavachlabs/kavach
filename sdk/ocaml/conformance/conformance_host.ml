(* The conformance host (SPEC §9.6): python3 spec/host/run.py --host <this exe>. *)
let () =
  Kavach.maybe_host Kavach.Conformance.handler;
  prerr_endline "conformance_host: run with kavach-host as the last argument";
  exit 2
