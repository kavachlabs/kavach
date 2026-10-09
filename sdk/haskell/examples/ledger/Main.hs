-- | Kavach's demo service, in Haskell: a single-writer wallet ledger that folds
-- JSON events into balances.
--
-- > ledger --in events.jsonl            # the buggy build: "amount": null crashes it
-- > ledger --fix --in events.jsonl      # the fixed build
--
-- The build is chosen by a command-line flag and not an environment variable
-- on purpose: environment variables are served from the journal on replay, so
-- a replay of the buggy build's fixture would otherwise run the buggy code.
-- Replay hosts are therefore @ledger@ (old) and @ledger --fix@ (new).
--
-- It records through @kavach-recorder@ (@$KAVACH_RECORDER@ or PATH).
module Main (main) where

import qualified Data.ByteString as BS
import qualified Data.ByteString.Char8 as BC
import Data.Int (Int64)
import Data.Map.Strict (Map)
import qualified Data.Map.Strict as M
import Data.Maybe (fromJust)
import Data.Text (Text)
import qualified Data.Text as T
import Data.Time.Format (defaultTimeLocale, formatTime)
import Kavach
import Kavach.Json
import Numeric (showHex)
import System.Environment (getArgs)
import System.Exit (exitFailure)
import System.IO

data Ledger = Ledger
  { balances :: !(Map Text Int64)
  , net :: !Int64 -- ^ deposits minus withdrawals
  }

data Event = Event
  { evId :: Text
  , evType :: Text
  , evAccount :: Text
  , evTo :: Text
  , evAmount :: Maybe Int64 -- ^ Nothing for a missing or null amount
  }

parseEvent :: BS.ByteString -> Either String Event
parseEvent bs = do
  j <- parseJson bs
  let str k = maybe "" id (field k j >>= asText)
  pure
    Event
      { evId = str "id"
      , evType = str "type"
      , evAccount = str "account"
      , evTo = str "to"
      , evAmount = fromInteger <$> (field "amount" j >>= asInt)
      }

ledgerHandler :: Bool -> Handler Ledger
ledgerHandler fixed =
  Handler
    { handle = handleEvent fixed
    , initial = Ledger M.empty 0
    , snapshot = Just snap
    , restore = Just unsnap
    , invariants =
        [ ( "balances_non_negative"
          , \l -> case [(a, b) | (a, b) <- M.toList (balances l), b < 0] of
              [] -> Right ()
              (a, b) : _ -> Left (T.pack ("account " ++ T.unpack a ++ " has balance " ++ show b))
          )
        , ( "money_conserved"
          , \l ->
              let total = sum (M.elems (balances l))
               in if total == net l
                    then Right ()
                    else Left (T.pack ("balances sum to " ++ show total ++ ", deposits minus withdrawals is " ++ show (net l)))
          )
        ]
    }
  where
    snap l = renderJson (JObj [("balances", JObj [(k, num v) | (k, v) <- M.toList (balances l)]), ("net", num (net l))])
    unsnap b = do
      j <- parseJson b
      bals <- case field "balances" j of
        Just (JObj kvs) -> traverse (\(k, v) -> maybe (Left "bad balance") (\n -> Right (k, fromInteger n)) (asInt v)) kvs
        _ -> Left "no balances"
      n <- maybe (Left "no net") Right (field "net" j >>= asInt)
      pure (Ledger (M.fromList bals) (fromInteger n))

num :: Int64 -> Json
num = JNum . show

handleEvent :: Bool -> Input -> Ledger -> Kavach Ledger
handleEvent fixed inp l = case parseEvent (inputData inp) of
  Left e -> kavachError (T.pack ("decode event at " ++ T.unpack (inputPosition inp) ++ ": " ++ e))
  Right ev
    | fixed, Nothing <- evAmount ev -> reject ev "missing amount" >> pure l
    | otherwise -> do
        -- The planted bug: with a null amount this is Maybe.fromJust: Nothing.
        let amount = fromJust (evAmount ev)
        if amount <= 0
          then reject ev "amount must be positive" >> pure l
          else do
            at <- now
            let stamp = T.pack (formatTime defaultTimeLocale "%Y-%m-%dT%H:%M:%S%QZ" at)
            case T.unpack (evType ev) of
              "deposit" -> post ev stamp (evAccount ev) amount l {net = net l + amount}
              "withdraw"
                | balance (evAccount ev) < amount -> reject ev "insufficient funds" >> pure l
                | otherwise -> post ev stamp (evAccount ev) (negate amount) l {net = net l - amount}
              "transfer"
                | balance (evAccount ev) < amount -> reject ev "insufficient funds" >> pure l
                | otherwise -> post ev stamp (evAccount ev) (negate amount) l >>= post ev stamp (evTo ev) amount
              _ -> reject ev ("unknown event type " <> evType ev) >> pure l
  where
    balance a = M.findWithDefault 0 a (balances l)

post :: Event -> Text -> Text -> Int64 -> Ledger -> Kavach Ledger
post ev stamp account delta l = do
  let bal = M.findWithDefault 0 account (balances l) + delta
  txn <- random 8
  emit "ledger.entries" $
    renderJson $
      JObj
        [ ("txn", JStr (T.pack (concatMap hex (BS.unpack txn))))
        , ("event", JStr (evId ev))
        , ("account", JStr account)
        , ("delta", num delta)
        , ("balance", num bal)
        , ("at", JStr stamp)
        ]
  pure l {balances = M.insert account bal (balances l)}
  where
    hex w = (if w < 16 then "0" else "") ++ showHex w ""

reject :: Event -> Text -> Kavach ()
reject ev reason = emit "ledger.rejections" (renderJson (JObj [("event", JStr (evId ev)), ("reason", JStr reason)]))

data Args = Args {fix :: Bool, inFile :: Maybe FilePath, fixtures :: FilePath}

parseArgs :: [String] -> Args
parseArgs = go (Args False Nothing "fixtures")
  where
    go a ("--fix" : r) = go a {fix = True} r
    go a ("--in" : f : r) = go a {inFile = Just f} r
    go a ("--fixtures" : d : r) = go a {fixtures = d} r
    go a (_ : r) = go a r
    go a [] = a

main :: IO ()
main = do
  args <- parseArgs <$> getArgs
  -- Lets the kavach CLI use this binary to replay fixtures.
  maybeHost (ledgerHandler (fix args))
  path <- maybe (hPutStrLn stderr "ledger: pass --in FILE" >> exitFailure) pure (inFile args)
  events <- filter (not . BS.null) . map BC.strip . BC.lines <$> BS.readFile path
  let opts =
        (defaultOptions "ledger")
          { roDir = Just (T.pack (fixtures args))
          , roDeliver = mapM_ (\o -> putStrLn (pad (T.unpack (outputSink o)) ++ " " ++ BC.unpack (outputData o)))
          }
      pad s = s ++ replicate (18 - length s) ' '
  withRecorder opts (ledgerHandler (fix args)) $ \r ->
    let loop _ [] = pure ()
        loop n (e : es) = do
          res <- step r (Input (T.pack ("file:" ++ path)) (T.pack (show n)) e)
          case stepFailure res of
            Just f | failKind f == "panic" -> do
              hPutStrLn stderr ("ledger: line " ++ show n ++ ": panic: " ++ T.unpack (failMessage f))
              closeRecorder r
              exitFailure
            Just f -> hPutStrLn stderr ("ledger: line " ++ show n ++ ": " ++ T.unpack (failKind f) ++ ": " ++ T.unpack (failMessage f)) >> loop (n + 1 :: Int) es
            Nothing -> loop (n + 1) es
     in loop 1 events
