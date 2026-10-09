(* The conformance handler of SPEC §9.6. *)

module H = struct
  type t = { mutable count : int }

  exception Bad of string

  let init () = { count = 0 }
  let trace env data = Env.emit env ~sink:"trace" data
  let obj fields = Json.to_string (Json.Obj fields)

  let run_op env st op =
    let field k conv =
      match Json.member k op with
      | Some v -> (try conv v with Failure _ -> raise (Bad ("bad field " ^ k)))
      | None -> raise (Bad ("missing field " ^ k))
    in
    let s k = field k Json.to_str in
    match s "op" with
    | "clock" -> trace env (obj [ ("clock", Json.String (Int64.to_string (Env.now_ns env))) ])
    | "rand" -> trace env (Env.random env (field "n" Json.to_int))
    | "gateway" -> (
        match Env.query env ~gateway:(s "gateway") (s "request") with
        | Ok r -> trace env r
        | Error e -> trace env (obj [ ("error", Json.String e) ]))
    | "config" -> (
        match Env.config env (s "key") with
        | Some v -> trace env v
        | None -> trace env (obj [ ("unset", Json.Bool true) ]))
    | "getenv" -> (
        match Sys.getenv_opt (s "name") with
        | Some v -> trace env v
        | None -> trace env (obj [ ("unset", Json.Bool true) ]))
    | "emit" -> Env.emit env ~sink:(s "sink") (s "data")
    | "panic" -> raise (Handler.Panic (s "message"))
    | "error" -> raise (Bad (s "message"))
    | "print" -> print_endline (s "text")
    | "count" -> st.count <- st.count + field "n" Json.to_int
    | op -> raise (Bad ("unknown op " ^ op))

  let handle env (input : Input.t) st =
    match Json.parse input.data with
    | exception Json.Parse_error m -> Error ("bad input: " ^ m)
    | Json.List ops -> (
        match List.iter (run_op env st) ops with
        | () -> st.count <- st.count + 1; Ok st
        | exception Bad m -> Error m)
    | _ -> Error "bad input: not an array"

  let snapshot = Some (fun st -> string_of_int st.count)
  let restore = Some (fun s -> { count = int_of_string s })

  let invariants =
    [ ("below_limit", fun st -> if st.count < 1000 then Ok () else Error (Printf.sprintf "count is %d" st.count)) ]
end

let handler : (module Handler.S) = (module H)
