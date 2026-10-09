-- | The conformance handler of SPEC §9.6. Test support: it is the one handler
-- that needs the SDK's private 'unsafeIO' (for @print@ and @getenv@).
module Kavach.Conformance
  ( conformance
  ) where

import qualified Data.ByteString as BS
import qualified Data.ByteString.Char8 as BC
import Data.Char (ord)
import Data.Text (Text)
import qualified Data.Text as T
import qualified Data.Text.Encoding as TE
import Kavach.Effects
import Kavach.Internal
import Kavach.Json
import Numeric (showHex)
import System.IO (hPutStrLn, stdout)
import qualified System.Posix.Env.ByteString as PosixEnv
import Text.Read (readMaybe)

-- | State is the count.
conformance :: Handler Int
conformance =
  Handler
    { handle = \inp c -> case parseJson (inputData inp) of
        Right (JArr ops) -> run c ops
        _ -> kavachError "data is not a JSON array"
    , initial = 0
    , snapshot = Just (BC.pack . show)
    , restore = Just (\b -> maybe (Left "not a decimal count") Right (readMaybe (BC.unpack b)))
    , invariants = [("below_limit", \c -> if c >= 1000 then Left (T.pack ("count is " ++ show c)) else Right ())]
    }

run :: Int -> [Json] -> Kavach Int
run c [] = pure (c + 1)
run c (op : rest) = case field "op" op >>= asText of
  Just "count" -> do
    n <- int "n"
    -- applies at once, even if a later operation fails the step
    commit (c + n)
    run (c + n) rest
  Just "clock" -> do
    ns <- nowNanos
    trace (compact [("clock", Str (T.pack (show ns)))]) >> next
  Just "rand" -> do
    n <- int "n"
    random n >>= trace >> next
  Just "gateway" -> do
    r <- query' =<< text "gateway"
    case r of
      Right resp -> trace resp
      Left (GatewayError e) -> trace (compact [("error", Str e)])
    next
  Just "config" -> do
    v <- text "key" >>= config
    trace (maybe (compact [("unset", JTrue)]) id v) >> next
  Just "getenv" -> do
    n <- text "name"
    v <- unsafeIO (PosixEnv.getEnv (TE.encodeUtf8 n))
    trace (maybe (compact [("unset", JTrue)]) id v) >> next
  Just "emit" -> do
    s <- text "sink"
    d <- text "data"
    emit s (TE.encodeUtf8 d) >> next
  Just "panic" -> text "message" >>= kavachPanic
  Just "error" -> text "message" >>= kavachError
  Just "print" -> do
    t <- text "text"
    unsafeIO (hPutStrLn stdout (T.unpack t)) >> next
  other -> kavachError (T.pack ("unknown operation " ++ show other))
  where
    next = run c rest
    trace = emit "trace"
    int k = maybe (kavachError (T.pack ("bad " ++ show k))) (pure . fromInteger) (field k op >>= asInt)
    text k = maybe (kavachError (T.pack ("bad " ++ show k))) pure (field k op >>= asText)
    query' g = do
      req <- text "request"
      query g (TE.encodeUtf8 req)

data V = Str Text | JTrue

-- | Compact JSON with exactly the escaping of §9.6.
compact :: [(Text, V)] -> BS.ByteString
compact kvs = TE.encodeUtf8 (T.concat ["{", T.intercalate "," [str k <> ":" <> val v | (k, v) <- kvs], "}"])
  where
    val (Str t) = str t
    val JTrue = "true"
    str t = T.concat ["\"", T.concatMap esc t, "\""]
    esc '"' = "\\\""
    esc '\\' = "\\\\"
    esc '\n' = "\\n"
    esc '\r' = "\\r"
    esc '\t' = "\\t"
    esc ch
      | ch < ' ' = T.pack ("\\u" ++ replicate (4 - length h) '0' ++ h)
      | otherwise = T.singleton ch
      where h = showHex (ord ch) ""
