type t =
  | Null
  | Bool of bool
  | Int of int
  | Float of float
  | String of string
  | List of t list
  | Obj of (string * t) list

exception Parse_error of string

(* Escapes exactly what SPEC §9.6 lists; everything else stays raw UTF-8. *)
let add_string b s =
  Buffer.add_char b '"';
  String.iter
    (function
      | '"' -> Buffer.add_string b "\\\""
      | '\\' -> Buffer.add_string b "\\\\"
      | '\n' -> Buffer.add_string b "\\n"
      | '\r' -> Buffer.add_string b "\\r"
      | '\t' -> Buffer.add_string b "\\t"
      | c when Char.code c < 0x20 -> Buffer.add_string b (Printf.sprintf "\\u%04x" (Char.code c))
      | c -> Buffer.add_char b c)
    s;
  Buffer.add_char b '"'

let rec write b = function
  | Null -> Buffer.add_string b "null"
  | Bool v -> Buffer.add_string b (string_of_bool v)
  | Int n -> Buffer.add_string b (string_of_int n)
  | Float f -> Buffer.add_string b (Printf.sprintf "%.17g" f)
  | String s -> add_string b s
  | List l ->
      Buffer.add_char b '[';
      List.iteri (fun i v -> if i > 0 then Buffer.add_char b ','; write b v) l;
      Buffer.add_char b ']'
  | Obj l ->
      Buffer.add_char b '{';
      List.iteri
        (fun i (k, v) ->
          if i > 0 then Buffer.add_char b ',';
          add_string b k;
          Buffer.add_char b ':';
          write b v)
        l;
      Buffer.add_char b '}'

let to_string v =
  let b = Buffer.create 64 in
  write b v;
  Buffer.contents b

let parse s =
  let n = String.length s and i = ref 0 in
  let fail m = raise (Parse_error (Printf.sprintf "%s at offset %d" m !i)) in
  let rec ws () =
    if !i < n && (match s.[!i] with ' ' | '\t' | '\n' | '\r' -> true | _ -> false) then (incr i; ws ())
  in
  let expect c = if !i < n && s.[!i] = c then incr i else fail (Printf.sprintf "expected '%c'" c) in
  let lit w v =
    let l = String.length w in
    if !i + l <= n && String.sub s !i l = w then (i := !i + l; v) else fail "bad literal"
  in
  let hex4 () =
    if !i + 4 > n then fail "short \\u escape";
    let v = ref 0 in
    for k = 0 to 3 do
      let d =
        match s.[!i + k] with
        | '0' .. '9' as c -> Char.code c - 48
        | 'a' .. 'f' as c -> Char.code c - 87
        | 'A' .. 'F' as c -> Char.code c - 55
        | _ -> fail "bad \\u escape"
      in
      v := (!v * 16) + d
    done;
    i := !i + 4;
    !v
  in
  let str () =
    expect '"';
    let b = Buffer.create 16 in
    let rec go () =
      if !i >= n then fail "unterminated string";
      let c = s.[!i] in
      incr i;
      match c with
      | '"' -> ()
      | '\\' ->
          if !i >= n then fail "unterminated escape";
          let e = s.[!i] in
          incr i;
          (match e with
          | '"' | '\\' | '/' -> Buffer.add_char b e
          | 'n' -> Buffer.add_char b '\n'
          | 'r' -> Buffer.add_char b '\r'
          | 't' -> Buffer.add_char b '\t'
          | 'b' -> Buffer.add_char b '\b'
          | 'f' -> Buffer.add_char b '\012'
          | 'u' ->
              let cp = hex4 () in
              let cp =
                if cp >= 0xD800 && cp < 0xDC00 && !i + 6 <= n && s.[!i] = '\\' && s.[!i + 1] = 'u' then (
                  let save = !i in
                  i := !i + 2;
                  let lo = hex4 () in
                  if lo >= 0xDC00 && lo < 0xE000 then 0x10000 + ((cp - 0xD800) lsl 10) + (lo - 0xDC00)
                  else (i := save; cp))
                else cp
              in
              Buffer.add_utf_8_uchar b (if Uchar.is_valid cp then Uchar.of_int cp else Uchar.rep)
          | _ -> fail "bad escape");
          go ()
      | c -> Buffer.add_char b c; go ()
    in
    go ();
    Buffer.contents b
  in
  let num () =
    let start = !i in
    while !i < n && (match s.[!i] with '0' .. '9' | '-' | '+' | '.' | 'e' | 'E' -> true | _ -> false) do incr i done;
    let tok = String.sub s start (!i - start) in
    match int_of_string_opt tok with
    | Some v -> Int v
    | None -> (match float_of_string_opt tok with Some f -> Float f | None -> fail "bad number")
  in
  let rec value () =
    ws ();
    if !i >= n then fail "unexpected end";
    match s.[!i] with
    | 'n' -> lit "null" Null
    | 't' -> lit "true" (Bool true)
    | 'f' -> lit "false" (Bool false)
    | '"' -> String (str ())
    | '[' ->
        incr i; ws ();
        if !i < n && s.[!i] = ']' then (incr i; List [])
        else
          let rec items acc =
            let v = value () in
            ws ();
            if !i < n && s.[!i] = ',' then (incr i; items (v :: acc)) else (expect ']'; List (List.rev (v :: acc)))
          in
          items []
    | '{' ->
        incr i; ws ();
        if !i < n && s.[!i] = '}' then (incr i; Obj [])
        else
          let rec fields acc =
            ws ();
            let k = str () in
            ws (); expect ':';
            let v = value () in
            ws ();
            if !i < n && s.[!i] = ',' then (incr i; fields ((k, v) :: acc)) else (expect '}'; Obj (List.rev ((k, v) :: acc)))
          in
          fields []
    | '-' | '0' .. '9' -> num ()
    | _ -> fail "unexpected character"
  in
  let v = value () in
  ws ();
  if !i <> n then fail "trailing characters";
  v

let member k = function Obj l -> List.assoc_opt k l | _ -> None

let to_int = function Int n -> n | _ -> failwith "Json.to_int: not an integer"
let to_str = function String s -> s | _ -> failwith "Json.to_str: not a string"
