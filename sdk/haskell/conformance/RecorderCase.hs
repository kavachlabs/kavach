-- | Runs one SDK recorder case (spec/recorder/sdk/README.md) through this SDK:
-- records the conformance handler with the fake recorder as the recorder
-- command, then reads the fake's verdict.
module RecorderCase
  ( runCase
  , specDir
  ) where

import Control.Exception (throwIO)
import qualified Data.ByteString as BS
import qualified Data.ByteString.Char8 as BC
import Data.IORef
import qualified Data.Map.Strict as M
import Data.Text (Text)
import qualified Data.Text as T
import qualified Data.Text.Encoding as TE
import Kavach
import Kavach.Conformance (conformance)
import Kavach.Json
import Kavach.Wire (base64Decode)
import System.Directory (getTemporaryDirectory, removeFile, doesFileExist)
import System.Environment (lookupEnv)
import System.Posix.Process (getProcessID)

-- | @$KAVACH_SPEC_DIR@, else the repository's @spec@ directory.
specDir :: IO FilePath
specDir = maybe "/Users/koustav/code/kavach-labs/kavach/spec" id <$> lookupEnv "KAVACH_SPEC_DIR"

die' :: String -> IO a
die' = ioError . userError

unwrap :: String -> Maybe a -> IO a
unwrap what = maybe (die' ("bad case: " ++ what)) pure

b64 :: Maybe Json -> IO BS.ByteString
b64 j = unwrap "base64" (j >>= asText >>= base64Decode . TE.encodeUtf8)

-- | Run a case; Nothing if it passes, else the error.
runCase :: FilePath -> FilePath -> IO (Maybe String)
runCase spec casePath = do
  raw <- BS.readFile casePath
  case_ <- either die' pure (parseJson raw)
  tmp <- getTemporaryDirectory
  pid <- getProcessID
  let resultPath = tmp ++ "/kavach-hs-case-" ++ show pid ++ ".json"
      fake = spec ++ "/recorder/sdk/fake_recorder.py"
  openObj <- unwrap "open" (field "open" case_)
  let txt k = unwrap (T.unpack k) (field k openObj >>= asText)
  service <- txt "service"
  start <- txt "start"
  snaps <- unwrap "snapshots" (field "snapshots" openObj >>= asBool)
  h <- case field "snapshot" case_ >>= asText of
    Nothing -> pure conformance
    Just s -> case maybe (Left "no restore") ($ TE.encodeUtf8 s) (restore conformance) of
      Right c -> pure conformance {initial = c}
      Left e -> die' e
  let flags = case field "flags" case_ of
        Just (JObj kvs) -> [(k, TE.encodeUtf8 v) | (k, JStr v) <- kvs]
        _ -> []
  answers <- newIORef (M.empty :: M.Map Text [Json])
  let pop kind = do
        m <- readIORef answers
        case M.lookup kind m of
          Just (a : rest) -> writeIORef answers (M.insert kind rest m) >> pure a
          _ -> die' ("the handler read a " ++ T.unpack kind ++ " the case has no answer for")
      opts =
        (defaultOptions service)
          { roStart = if start == "snapshot" then FromSnapshot else Genesis
          , roSnapshots = Just snaps
          , roRecorderCommand = Just ["python3", fake, casePath, resultPath]
          , roGateways = const $ Just $ Gateway Remote $ \_ -> do
              a <- pop "gateway"
              case field "error" a >>= asText of
                Just e -> throwIO (GatewayError e)
                Nothing -> b64 (field "response" a)
          , roConfig = \_ -> do
              a <- pop "config"
              if field "unset" a == Just (JBool True) then pure Nothing else Just <$> b64 (field "value" a)
          , roConfigSource = "case"
          , roFlags = pure flags
          , roClock = do
              a <- pop "clock"
              unwrap "clock" (fromInteger <$> (asText a >>= \t -> asInt (JNum (T.unpack t))))
          , roRandom = \n -> do
              d <- pop "rand" >>= b64 . Just
              if BS.length d == n then pure d else die' "case rand answer has the wrong length"
          , roRequired = True
          }
  r <- newRecorder opts h
  actions <- case field "actions" case_ of
    Just (JArr as) -> pure as
    _ -> die' "bad case: actions"
  mapM_ (act r answers) actions
  closeRecorder r
  ok <- doesFileExist resultPath
  if not ok
    then pure (Just "the fake recorder wrote no result (did the SDK close it?)")
    else do
      res <- BS.readFile resultPath >>= either die' pure . parseJson
      removeFile resultPath
      pure $
        if field "pass" res == Just (JBool True)
          then Nothing
          else Just (maybe "failed" T.unpack (field "error" res >>= asText))
  where
    act r answers a
      | Just s <- field "step" a = do
          let g k = maybe "" id (field k s >>= asText)
          d <- b64 (field "data" s)
          writeIORef answers $ case field "answers" a of
            Just (JObj kvs) -> M.fromList [(k, vs) | (k, JArr vs) <- kvs]
            _ -> M.empty
          _ <- step r (Input (g "source") (g "position") d)
          pure ()
      | Just f <- field "flush" a = do
          _ <- flush r (field "durable" f == Just (JBool True))
          pure ()
      | otherwise = die' ("bad case action: " ++ BC.unpack (renderJson a))
