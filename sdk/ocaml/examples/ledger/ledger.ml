(* Kavach's demo service, in OCaml: a single-writer wallet ledger.

     ledger.exe --in events.jsonl          the buggy build: panics on "amount": null
     ledger.exe --fix --in events.jsonl    the fixed build

   The build is chosen by a command-line flag and not an environment variable:
   environment variables are served from the journal on replay, so a replay of
   the buggy build's fixture would otherwise run the buggy code. Replay hosts
   are therefore `ledger.exe` (old) and `ledger.exe --fix` (new).

   If kavach-recorder is available (KAVACH_RECORDER or PATH) the service
   records through it; otherwise it says so loudly and runs unrecorded. *)
open Kavach

let fix = Array.exists (( = ) "--fix") Sys.argv

(* RFC 3339 with nanoseconds, trailing zeros trimmed, as Go's RFC3339Nano. *)
let rfc3339 ns =
  let secs = Int64.to_float (Int64.div ns 1_000_000_000L) and frac = Int64.to_int (Int64.rem ns 1_000_000_000L) in
  let tm = Unix.gmtime secs in
  let frac =
    if frac = 0 then ""
    else
      let s = Printf.sprintf "%09d" frac in
      let n = ref 9 in
      while s.[!n - 1] = '0' do decr n done;
      "." ^ String.sub s 0 !n
  in
  Printf.sprintf "%04d-%02d-%02dT%02d:%02d:%02d%sZ" (tm.tm_year + 1900) (tm.tm_mon + 1) tm.tm_mday tm.tm_hour tm.tm_min
    tm.tm_sec frac

module Ledger : HANDLER = struct
  type t = { balances : (string, int) Hashtbl.t; mutable net : int }  (* net: deposits minus withdrawals *)

  let init () = { balances = Hashtbl.create 16; net = 0 }
  let balance st a = Option.value (Hashtbl.find_opt st.balances a) ~default:0
  let get k ev = Option.value (Json.member k ev) ~default:Json.Null

  let post env st ev account delta at =
    Hashtbl.replace st.balances account (balance st account + delta);
    let txn = String.concat "" (List.map (fun c -> Printf.sprintf "%02x" (Char.code c)) (List.of_seq (String.to_seq (Env.random env 8)))) in
    Env.emit env ~sink:"ledger.entries"
      (Json.to_string
         (Json.Obj
            [ ("txn", String txn); ("event", get "id" ev); ("account", String account); ("delta", Int delta);
              ("balance", Int (balance st account)); ("at", String at) ]))

  let reject env ev reason =
    Env.emit env ~sink:"ledger.rejections"
      (Json.to_string (Json.Obj [ ("event", get "id" ev); ("reason", String reason) ]))

  let handle env (input : Input.t) st =
    match Json.parse input.data with
    | exception Json.Parse_error m -> Error (Printf.sprintf "decode event at %s: %s" input.position m)
    | ev ->
        if fix && get "amount" ev = Json.Null then reject env ev "missing amount"
        else begin
          (* The planted bug: Json.to_int raises on null. *)
          let amount = Json.to_int (get "amount" ev) in
          if amount <= 0 then reject env ev "amount must be positive"
          else
            let at = rfc3339 (Env.now_ns env) in
            let account = Json.to_str (get "account" ev) in
            match Json.to_str (get "type" ev) with
            | "deposit" -> st.net <- st.net + amount; post env st ev account amount at
            | "withdraw" ->
                if balance st account < amount then reject env ev "insufficient funds"
                else (st.net <- st.net - amount; post env st ev account (-amount) at)
            | "transfer" ->
                if balance st account < amount then reject env ev "insufficient funds"
                else (post env st ev account (-amount) at; post env st ev (Json.to_str (get "to" ev)) amount at)
            | other -> reject env ev ("unknown event type " ^ other)
        end;
        Ok st

  let sorted st = List.sort compare (Hashtbl.fold (fun a b acc -> (a, b) :: acc) st.balances [])

  let snapshot =
    Some
      (fun st ->
        Json.to_string
          (Json.Obj [ ("balances", Json.Obj (List.map (fun (a, b) -> (a, Json.Int b)) (sorted st))); ("net", Int st.net) ]))

  let restore =
    Some
      (fun s ->
        let j = Json.parse s in
        let st = init () in
        (match Json.member "balances" j with
        | Some (Json.Obj l) -> List.iter (fun (a, b) -> Hashtbl.replace st.balances a (Json.to_int b)) l
        | _ -> ());
        st.net <- Json.to_int (get "net" j);
        st)

  let invariants =
    [ ( "balances_non_negative",
        fun st ->
          match List.find_opt (fun (_, b) -> b < 0) (sorted st) with
          | Some (a, b) -> Error (Printf.sprintf "account %s has balance %d" a b)
          | None -> Ok () );
      ( "money_conserved",
        fun st ->
          let sum = Hashtbl.fold (fun _ b acc -> acc + b) st.balances 0 in
          if sum = st.net then Ok ()
          else Error (Printf.sprintf "balances sum to %d, deposits minus withdrawals is %d" sum st.net) ) ]
end

let () =
  (* Lets the kavach CLI use this binary to replay fixtures. *)
  Kavach.maybe_host (module Ledger);
  Printexc.record_backtrace true;
  let path = ref "events.jsonl" and dir = ref "fixtures" in
  Arg.parse
    [ ("--fix", Arg.Unit ignore, " run the fixed handler");
      ("--in", Arg.Set_string path, "FILE JSON-lines file of events");
      ("--fixtures", Arg.Set_string dir, "DIR directory for journals and crash fixtures") ]
    (fun a -> raise (Arg.Bad ("unexpected argument " ^ a)))
    "ledger [--fix] [--in FILE] [--fixtures DIR]";
  let deliver outs =
    List.iter (fun (o : Recorder.output) -> Printf.printf "%-18s %s\n%!" o.sink o.data) outs
  in
  let rec_ = Recorder.create ~service:"ledger" ~dir:!dir ~deliver (module Ledger) in
  let source = "file:" ^ Filename.basename !path in
  let status = ref 0 in
  (try
     In_channel.with_open_bin !path (fun ic ->
         let line_no = ref 0 in
         try
           while true do
             let line = input_line ic in
             incr line_no;
             if String.trim line <> "" then
               match Recorder.step rec_ { source; position = string_of_int !line_no; data = line } with
               | Ok () -> ()
               | Error { kind = "panic"; message; _ } ->
                   (* Stop the service, as the Go demo does; the recorder has
                      already marked the failure and cut a fixture. *)
                   Printf.eprintf "ledger: line %d: panic: %s\n%!" !line_no message;
                   status := 2;
                   raise Exit
               | Error f -> Printf.eprintf "ledger: line %d: %s: %s\n%!" !line_no f.kind f.message
           done
         with End_of_file -> ())
   with Exit -> ());
  Recorder.close rec_;
  exit !status
