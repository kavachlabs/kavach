-- | The host side of the replay protocol (SPEC §9).
module Kavach.Host
  ( maybeHost
  , runHost
  ) where

import Control.Exception hiding (Handler, handle)
import qualified Control.Exception as E
import Control.Monad (unless, when)
import qualified Data.ByteString as BS
import Data.ByteString (ByteString)
import qualified Data.ByteString.Char8 as BC
import Data.Dynamic (fromDynamic)
import Data.IORef
import Data.Maybe (fromMaybe)
import Data.Text (Text)
import qualified Data.Text as T
import qualified Data.Text.Encoding as TE
import Data.Typeable (Typeable)
import Kavach.Internal
import Kavach.Json
import Kavach.Recorder (findRecorder)
import Kavach.Wire
import System.Environment (getArgs)
import System.Exit (ExitCode (..), exitWith)
import GHC.IO.Handle (hDuplicate, hDuplicateTo)
import System.IO
import System.IO.Error (isEOFError)
import System.Process (readCreateProcessWithExitCode, proc)
import System.Timeout (timeout)

-- | The protocol was violated; the host sends @fatal@ and stops.
newtype Fatal = Fatal String deriving (Show)

instance Exception Fatal

-- | The driver closed the pipe.
data Gone = Gone deriving (Show)

instance Exception Gone

-- | If this process was started as a replay host (its last argument is
-- @kavach-host@), serve the driver and exit; otherwise return at once. Call
-- it first thing in @main@, before consuming any input.
--
-- The protocol stream is taken before the handler exists: stdout is pointed at
-- stderr, so a handler that prints cannot corrupt it (SPEC §9.1).
maybeHost :: Typeable s => Handler s -> IO ()
maybeHost h = do
  args <- getArgs
  unless (null args || last args /= "kavach-host") $ do
    out <- hDuplicate stdout
    hDuplicateTo stderr stdout
    hSetEncoding stdout utf8
    hSetBinaryMode out True
    hSetBuffering out (BlockBuffering Nothing)
    hSetBinaryMode stdin True
    code <- runHost h stdin out
    hFlush stdout
    hFlush stderr
    exitWith code

send :: Handle -> Json -> IO ()
send w msg = do
  r <- try (BS.hPut w (renderJson msg <> "\n") >> hFlush w)
  case r of
    Left e -> const (throwIO Gone) (e :: IOException)
    Right () -> pure ()

recv :: Handle -> IO (Text, Json)
recv rd = do
  r <- try (BC.hGetLine rd)
  line <- case r of
    Left e | isEOFError e -> throwIO Gone
           | otherwise -> throwIO (Fatal ("could not read: " ++ show e))
    Right l -> pure l
  case parseJson line of
    Right msg | Just t <- field "t" msg >>= asText -> pure (t, msg)
    Right _ -> throwIO (Fatal "malformed message: no type")
    Left _ -> throwIO (Fatal "malformed message: not JSON")

unb64 :: Maybe Json -> IO ByteString
unb64 j = case j >>= asText >>= base64Decode . TE.encodeUtf8 of
  Just b -> pure b
  Nothing -> throwIO (Fatal "expected a base64 string")

b64 :: ByteString -> Json
b64 = JStr . TE.decodeUtf8 . base64Encode

obj :: [(Text, Json)] -> Json
obj = JObj

-- | @ready.environment@: @kavach-recorder facts@ if it can be run, plus
-- @host.runtime@ (§9.2).
collectEnvironment :: IO Json
collectEnvironment = do
  cmd <- findRecorder Nothing
  facts <- case cmd of
    Just (c : args) -> do
      r <- try (timeout 10000000 (readCreateProcessWithExitCode (proc c (args ++ ["facts"])) ""))
      pure $ case r :: Either SomeException (Maybe (ExitCode, String, String)) of
        Right (Just (ExitSuccess, o, _)) | Right (JObj kvs) <- parseJson (BC.pack o) -> kvs
        _ -> []
    _ -> pure []
  pure (JObj (filter ((/= "host.runtime") . fst) facts ++ [("host.runtime", obj [("value", b64 (BC.pack runtimeName))])]))

-- | Serve one driver session over a pair of handles. Returns the exit status.
runHost :: Typeable s => Handler s -> Handle -> Handle -> IO ExitCode
runHost h rd wr = do
  st <- newIORef Nothing
  let loop = do
        r <- try (recv rd)
        case r of
          Left Gone -> pure ExitSuccess
          Right (t, msg) -> case T.unpack t of
            "hello" -> hello st msg >> loop
            "step" -> do
              s <- readIORef st
              maybe (throwIO (Fatal "step before hello")) (\s' -> hostStep h rd wr st s' msg) s
              loop
            "end" -> pure ExitSuccess
            "abort" -> loop
            other -> throwIO (Fatal ("unexpected message " ++ show other))
  loop `catches`
    [ E.Handler (\(Fatal m) -> do
        _ <- try (send wr (obj [("t", JStr "fatal"), ("message", JStr (T.pack m))])) :: IO (Either Gone ())
        pure (ExitFailure 1))
    , E.Handler (\Gone -> pure (ExitFailure 1))
    ]
  where
    hello st msg = do
      when (field "protocol" msg /= Just (JNum "1")) $ throwIO (Fatal "unsupported protocol")
      when (field "mode" msg == Just (JStr "sandbox")) $ throwIO (Fatal "sandbox mode not supported")
      s <-
        if field "start" msg == Just (JStr "snapshot")
          then case restore h of
            Nothing -> throwIO (Fatal "the journal starts from a snapshot but the handler has no restore")
            Just f -> do
              bytes <- unb64 (field "snapshot" msg)
              either (\e -> throwIO (Fatal ("could not restore the snapshot: " ++ e))) pure (f bytes)
          else pure (initial h)
      writeIORef st (Just s)
      env <- collectEnvironment
      send wr $
        obj
          [ ("t", JStr "ready")
          , ("protocol", JNum "1")
          , ("sdk", JStr (T.pack producer))
          , ("invariants", JArr [JStr n | (n, _) <- invariants h])
          , ("environment", env)
          ]

hostStep :: Typeable s => Handler s -> Handle -> Handle -> IORef (Maybe s) -> s -> Json -> IO ()
hostStep h rd wr st s0 msg = do
  d <- unb64 (field "data" msg)
  let txt k = fromMaybe "" (field k msg >>= asText)
      inp = Input (txt "source") (txt "position") d
  aborted <- newIORef False
  ckpt <- newIORef Nothing
  let request m want = do
        a <- readIORef aborted
        when a $ throwIO KavachAbort
        send wr m
        (t, ans) <- recv rd
        if t == "abort"
          then writeIORef aborted True >> throwIO KavachAbort
          else do
            when (t /= want) $ throwIO (Fatal ("expected a " ++ show want ++ " answer, got " ++ show t))
            pure ans
      env =
        EnvImpl
          { envClock = do
              a <- request (obj [("t", JStr "clock")]) "clock"
              maybe (throwIO (Fatal "bad clock answer")) (pure . fromInteger) (field "unix_nanos" a >>= asText >>= readInt)
          , envRandom = \n -> do
              a <- request (obj [("t", JStr "rand"), ("n", JNum (show n))]) "rand"
              b <- unb64 (field "data" a)
              when (BS.length b /= n) $ throwIO (Fatal "rand answer has the wrong length")
              pure b
          , envQuery = \g req -> do
              a <- request (obj [("t", JStr "gateway"), ("gateway", JStr g), ("request", b64 req), ("scope", JStr "remote")]) "gateway"
              when (field "live" a == Just (JBool True)) $ throwIO (Fatal "live gateway queries are not supported")
              case field "error" a >>= asText of
                Just e | not (T.null e) -> pure (Left (GatewayError e))
                _ -> Right <$> unb64 (field "response" a)
          , envConfig = \k -> do
              a <- request (obj [("t", JStr "config"), ("key", JStr k)]) "config"
              if field "present" a == Just (JBool True) then Just <$> unb64 (field "value" a) else pure Nothing
          , envEmit = \sc sink dat -> do
              a <- readIORef aborted
              when a $ throwIO KavachAbort
              send wr (obj [("t", JStr "emit"), ("sink", JStr sink), ("data", b64 dat), ("scope", JStr (if sc == Local then "local" else "remote"))])
          , envCommit = \dy -> writeIORef ckpt (fromDynamic dy)
          }
  r <- runStepOrFatal h env inp s0
  wasAborted <- readIORef aborted
  if wasAborted
    then done [("outcome", JStr "aborted")]
    else do
      failure <- case r of
        Right s' -> do
          writeIORef st (Just s')
          checkInvariants h s'
        Left (f, _) -> do
          kept <- readIORef ckpt
          writeIORef st (Just (fromMaybe s0 kept))
          pure (Just f)
      case failure of
        Nothing -> done [("outcome", JStr "ok")]
        Just f ->
          done $
            [("outcome", JStr (failKind f)), ("message", JStr (failMessage f))]
              ++ [("detail", JStr (failDetail f)) | not (T.null (failDetail f))]
  where
    done fields = send wr (obj (("t", JStr "done") : fields))
    readInt t = case reads (T.unpack t) of
      [(n, "")] -> Just (n :: Integer)
      _ -> Nothing

-- | Like 'runStep', but protocol exceptions and aborts pass through rather than
-- becoming handler failures.
runStepOrFatal :: Handler s -> EnvImpl -> Input -> s -> IO (Either (Failure, SomeException) s)
runStepOrFatal h env inp s0 = do
  r <- runStep h env inp s0
  case r of
    Left (_, e)
      | Just (Fatal m) <- fromException e -> throwIO (Fatal m)
      | Just Gone <- fromException e -> throwIO Gone
    _ -> pure r
