open Kavach

let failures = ref 0

let check name cond =
  if not cond then (incr failures; Printf.printf "FAIL  %s\n%!" name)

let input data : Input.t = { source = "test"; position = "0"; data }

(* A handler whose handle runs [f]. *)
let handler f : (module HANDLER) =
  (module struct
    type t = unit

    let init () = ()
    let handle _ _ () = f ()
    let snapshot = None
    let restore = None
    let invariants = []
  end)

let env : Env.t =
  { clock = (fun () -> 0L); rand = (fun _ -> ""); query = (fun ~gateway:_ _ -> Error "x");
    config = (fun _ -> None); emit = (fun ~local:_ ~sink:_ _ -> ()) }

let run f =
  match Handler.instantiate (handler f) ~snapshot:None with
  | Ok i -> i.run env (input "")
  | Error m -> failwith m

let () =
  check "base64 vectors"
    (List.for_all
       (fun (a, b) -> Base64.encode a = b && Base64.decode b = a)
       [ ("", ""); ("f", "Zg=="); ("fo", "Zm8="); ("foo", "Zm9v"); ("foob", "Zm9vYg=="); ("\x01\x02\xff", "AQL/") ]);
  check "json escaping"
    (Json.to_string (Json.String "a\"b\\c\n\r\t\x01\x1f<>&\xc3\xa9") = "\"a\\\"b\\\\c\\n\\r\\t\\u0001\\u001f<>&\xc3\xa9\"");
  check "json round trip"
    (Json.parse "{\"a\":[1,-2,3.5,true,null,\"x\\u00e9\\ud83d\\ude00\"],\"b\":{}}"
    = Json.Obj
        [ ("a", Json.List [ Int 1; Int (-2); Float 3.5; Bool true; Null; String "x\xc3\xa9\xf0\x9f\x98\x80" ]);
          ("b", Json.Obj []) ]);
  check "json rejects trailing" (match Json.parse "{} x" with exception Json.Parse_error _ -> true | _ -> false);
  check "uvarint"
    (let b = Buffer.create 4 in
     Wire.uvarint b 300;
     Buffer.contents b = "\xac\x02");

  (* Failure mapping (README). *)
  (match run (fun () -> raise (Panic "boom")) with
  | Some { kind = "panic"; message = "boom"; _ } -> ()
  | _ -> check "Panic message is exact" false);
  (match run (fun () -> Error "bad") with
  | Some { kind = "error"; message = "bad"; _ } -> ()
  | _ -> check "Error message is exact" false);
  (match run (fun () -> failwith "oops") with
  | Some { kind = "panic"; message; _ } -> check "exception message" (message = Printexc.to_string (Failure "oops"))
  | _ -> check "exception is a panic" false);

  (* SPEC §4.5: no source locations in the message; they go to data. *)
  let location_free name f expected =
    match run f with
    | Some { kind = "panic"; message; detail } ->
        check (name ^ " message") (message = expected);
        check (name ^ " keeps its location in data") (String.length detail > 0 && String.contains detail ':')
    | _ -> check (name ^ " is a panic") false
  in
  location_free "Assert_failure" (fun () -> assert (Sys.opaque_identity false); Ok ()) "Assert_failure";
  location_free "Match_failure"
    (fun () -> raise (Match_failure ("src/file.ml", 12, 3)))
    "Match_failure";

  (* A recorder that cannot start never fails the step; required makes it fail. *)
  let missing = [| "/nonexistent/kavach-recorder" |] in
  let r = Recorder.create ~recorder_command:missing ~service:"t" (handler (fun () -> Ok ())) in
  check "unrecorded step still runs" (Recorder.step r (input "") = Ok ());
  check "not recording" (not (Recorder.recording r));
  Recorder.close r;
  check "required fails to start"
    (match Recorder.create ~recorder_command:missing ~required:true ~service:"t" (handler (fun () -> Ok ())) with
    | exception Recorder.Recorder_error _ -> true
    | _ -> false);
  if !failures > 0 then exit 1 else print_endline "unit tests passed"
