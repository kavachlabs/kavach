-- | The SDK half of the flight recorder pipe (SPEC §3.6, §10).
--
-- Recording never fails a step: if @kavach-recorder@ cannot be started, dies
-- or reports a fatal error, the problem goes to stderr, recording stops and
-- steps carry on unrecorded. With 'roRequired' a recorder that cannot be
-- started makes 'newRecorder' throw 'RecorderError' instead.
module Kavach.Recorder
  ( RecorderOptions (..)
  , Start (..)
  , Gateway (..)
  , Recorder
  , StepResult (..)
  , RecorderError (..)
  , defaultOptions
  , newRecorder
  , withRecorder
  , step
  , flush
  , closeRecorder
  , stepOk
  , findRecorder
  ) where

import Control.Concurrent (forkIO, threadDelay)
import Control.Concurrent.MVar
import Control.Concurrent.STM
import Control.Exception hiding (Handler, handle)
import Control.Monad (unless, void, when)
import qualified Data.ByteString as BS
import Data.ByteString (ByteString)
import qualified Data.ByteString.Char8 as BC
import Data.Dynamic (fromDynamic)
import Data.Int (Int64)
import Data.IORef
import Data.Maybe (fromMaybe, isJust, isNothing)
import Data.Text (Text)
import qualified Data.Text as T
import qualified Data.Text.Encoding as TE
import Data.Typeable (Typeable)
import Kavach.Internal
import Kavach.Json
import Kavach.Ring
import Kavach.Wire
import System.Directory (findExecutable)
import System.Environment (lookupEnv)
import System.IO
import System.IO.Error (isEOFError)
import System.Process
import System.Timeout (timeout)

data Start = Genesis | FromSnapshot deriving (Eq, Show)

-- | A registered gateway: the connection that makes the query, and its scope.
-- The connection may throw 'GatewayError', or any exception, to report a
-- failure; its text is recorded as the gateway's error.
data Gateway = Gateway
  { gwScope :: Scope
  , gwConnection :: ByteString -> IO ByteString
  }

data RecorderOptions = RecorderOptions
  { roService :: Text
  , roStart :: Start
  , roSnapshots :: Maybe Bool
  -- ^ whether to answer @snapshot_request@; default: the handler has snapshot and restore
  , roDeliver :: [Output] -> IO ()
  -- ^ receives a successful step's outputs
  , roGateways :: Text -> Maybe Gateway
  , roConfig :: Text -> IO (Maybe ByteString)
  , roConfigSource :: Text
  , roFlags :: IO [(Text, ByteString)]
  , roRecorderCommand :: Maybe [String]
  -- ^ recorder argument vector; else @$KAVACH_RECORDER@, else @kavach-recorder@ on PATH
  , roNoRing :: Bool
  -- ^ carry the record stream over the recorder's pipe instead of the shared-memory ring (§10.7)
  , roRingBytes :: Maybe Int
  -- ^ ring capacity: a power of two of at least 64 KiB; default 8 MiB
  , roRequired :: Bool
  , roHandlerId :: Maybe Text
  , roDir :: Maybe Text
  , roCompression :: Maybe Text
  , roLevel :: Maybe Int
  , roBlockBytes :: Maybe Int
  , roFlushMs :: Maybe Int
  , roSegmentBytes :: Maybe Int
  , roSegmentSeconds :: Maybe Int
  , roRetainSegments :: Maybe Int
  , roSecretKeys :: [Text]
  , roOnFixture :: Json -> IO ()
  , roClock :: IO Int64
  , roRandom :: Int -> IO ByteString
  , roReadyTimeout :: Double
  , roCloseTimeout :: Double
  }

-- | Live defaults: the real clock, @\/dev\/urandom@, config from environment
-- variables, no gateways, outputs dropped.
defaultOptions :: Text -> RecorderOptions
defaultOptions service =
  RecorderOptions
    { roService = service
    , roStart = Genesis
    , roSnapshots = Nothing
    , roDeliver = const (pure ())
    , roGateways = const Nothing
    , roConfig = \k -> fmap (TE.encodeUtf8 . T.pack) <$> lookupEnv (T.unpack k)
    , roConfigSource = "env"
    , roFlags = pure []
    , roRecorderCommand = Nothing
    , roNoRing = False
    , roRingBytes = Nothing
    , roRequired = False
    , roHandlerId = Nothing
    , roDir = Nothing
    , roCompression = Nothing
    , roLevel = Nothing
    , roBlockBytes = Nothing
    , roFlushMs = Nothing
    , roSegmentBytes = Nothing
    , roSegmentSeconds = Nothing
    , roRetainSegments = Nothing
    , roSecretKeys = []
    , roOnFixture = const (pure ())
    , roClock = realtimeNanos
    , roRandom = \n -> withBinaryFile "/dev/urandom" ReadMode (`BS.hGet` n)
    , roReadyTimeout = 10
    , roCloseTimeout = 10
    }

-- | The recorder could not be started and 'roRequired' was set.
newtype RecorderError = RecorderError String deriving (Show)

instance Exception RecorderError

data StepResult = StepResult
  { stepFailure :: Maybe Failure
  , stepException :: Maybe SomeException
  , stepOutputs :: [Output]
  }

stepOk :: StepResult -> Bool
stepOk = isNothing . stepFailure

data Recorder s = Recorder
  { rHandler :: Handler s
  , rOpts :: RecorderOptions
  , rState :: IORef s
  , rLock :: MVar ()
  , rIn :: Maybe Handle
  , rProc :: Maybe ProcessHandle
  , rRing :: Maybe Ring
  , rActive :: TVar Bool
  , rClosing :: TVar Bool
  , rClosed :: IORef Bool
  , rDurable :: TVar Int
  , rSnapReq :: TVar Bool
  , rReady :: TVar Bool
  , rAck :: TVar Bool
  , rLogged :: IORef Bool
  , rSnapshots :: Bool
  }

logLine :: String -> IO ()
logLine s = hPutStrLn stderr ("kavach: " ++ s)

-- | The recorder argument vector: the given command, else @$KAVACH_RECORDER@,
-- else @kavach-recorder@ on PATH.
findRecorder :: Maybe [String] -> IO (Maybe [String])
findRecorder (Just c) = pure (Just c)
findRecorder Nothing = do
  e <- lookupEnv "KAVACH_RECORDER"
  case e of
    Just p | not (null p) -> pure (Just [p])
    _ -> fmap (: []) <$> findExecutable "kavach-recorder"

-- | Stop recording, loudly, once. Never throws.
failRec :: Recorder s -> String -> IO ()
failRec r reason = do
  was <- atomically (swapTVar (rActive r) False)
  closing <- readTVarIO (rClosing r)
  logged <- readIORef (rLogged r)
  when (not logged && (was || not closing)) $ do
    writeIORef (rLogged r) True
    logLine (reason ++ "; recording has stopped and steps run unrecorded")

-- | Write to the recorder's standard input.
pipeWrite :: Recorder s -> ByteString -> IO ()
pipeWrite r bs = do
  active <- readTVarIO (rActive r)
  case rIn r of
    Just h | active && not (BS.null bs) -> do
      res <- try (BS.hPut h bs >> hFlush h)
      case res of
        Left e -> failRec r ("could not write to the recorder: " ++ show (e :: IOException))
        Right () -> pure ()
    _ -> pure ()

-- | Hand whole frames to the recorder, through the ring or the pipe.
rawWrite :: Recorder s -> ByteString -> IO ()
rawWrite r bs = case rRing r of
  Nothing -> pipeWrite r bs
  Just g -> publish r g bs

-- | 'rawWrite' for an encoded step: over the ring it is written in place, with
-- no intermediate buffer, unless it would wrap or the ring is nearly full.
rawWriteEnc :: Recorder s -> Enc -> IO ()
rawWriteEnc r enc@(Enc n w) = case rRing r of
  Nothing -> rawWrite r (encBytes enc)
  Just g -> do
    active <- readTVarIO (rActive r)
    when active $ do
      done <- tryPublishDirect g n w
      case done of
        Just used -> afterPublish r g used
        Nothing -> publish r g (encBytes enc)

afterPublish :: Recorder s -> Ring -> Int -> IO ()
afterPublish r g used = do
  let half = ringCap g `div` 2
  belled <- readIORef (ringBelled g)
  if used > half
    then unless belled (writeIORef (ringBelled g) True >> pipeWrite r (BS.singleton 1))
    else when belled (writeIORef (ringBelled g) False)

publish :: Recorder s -> Ring -> ByteString -> IO ()
publish r g b = do
  active <- readTVarIO (rActive r)
  unless (BS.null b || not active) $ do
    (n, used) <- tryPublish g b
    if n == 0
      then do
        -- Full: the recorder drains the ring on the doorbell. If it has
        -- exited, the control loop stops recording and ends the wait.
        pipeWrite r (BS.singleton 1)
        wait
      else afterPublish r g used >> publish r g (BS.drop n b)
  where
    wait = do
      active <- readTVarIO (rActive r)
      when active $ do
        (n, _) <- tryPublish g b
        if n == 0 then threadDelay 20 >> wait else publish r g (BS.drop n b)

-- | Wake a recorder that reads the ring (§10.7).
bell :: Recorder s -> IO ()
bell r = when (isJust (rRing r)) (pipeWrite r (BS.singleton 1))

newRecorder :: Typeable s => RecorderOptions -> Handler s -> IO (Recorder s)
newRecorder opts h = do
  when (roStart opts == FromSnapshot && not (isJust (snapshot h))) $
    ioError (userError "kavach: start FromSnapshot needs a handler with snapshot and restore")
  let canSnap = isJust (snapshot h) && isJust (restore h)
      snaps = fromMaybe canSnap (roSnapshots opts)
  when (snaps && not canSnap) $
    ioError (userError "kavach: snapshots need a handler with snapshot and restore")
  cmd <- findRecorder (roRecorderCommand opts)
  ring <-
    if roNoRing opts
      then pure Nothing
      else do
        res <- try (createRing (fromMaybe defaultRingBytes (roRingBytes opts)))
        case res of
          Right g -> pure (Just g)
          Left e -> do
            logLine ("no shared-memory ring, recording over the pipe: " ++ displayException (e :: IOException))
            pure Nothing
  spawned <- try (maybe (throwIO (RecorderError "kavach-recorder not found (set roRecorderCommand, $KAVACH_RECORDER or put it on PATH)")) spawn cmd)
  active <- newTVarIO (either (const False) (const True) (spawned :: Either SomeException (Handle, Handle, ProcessHandle)))
  closing <- newTVarIO False
  durable <- newTVarIO 0
  snapReq <- newTVarIO False
  ready <- newTVarIO False
  ack <- newTVarIO False
  closed <- newIORef False
  logged <- newIORef False
  st <- newIORef (initial h)
  lock <- newMVar ()
  when (either (const True) (const False) spawned) $ mapM_ destroyRing ring
  let (inH, proc') = case spawned of
        Right (i, _, p) -> (Just i, Just p)
        Left _ -> (Nothing, Nothing)
      r =
        Recorder
          { rHandler = h, rOpts = opts, rState = st, rLock = lock, rIn = inH, rProc = proc', rRing = ring
          , rActive = active, rClosing = closing, rClosed = closed, rDurable = durable
          , rSnapReq = snapReq, rReady = ready, rAck = ack, rLogged = logged, rSnapshots = snaps
          }
  case spawned of
    Right (_, out, _) -> void (forkIO (controlLoop r out))
    Left _ -> pure ()
  startup <- try $ do
    either throwIO (const (pure ())) spawned
    pipeWrite r (frameOpen (renderJson (openObject opts snaps ring)))
    flags <- roFlags opts
    rawWrite r (frameFacts (("host.runtime", BC.pack runtimeName) : flags))
    when (roStart opts == FromSnapshot) $ do
      s <- readIORef st
      rawWrite r . frameSnapshot =<< evaluate (maybe BS.empty ($ s) (snapshot h))
    when (roRequired opts) $ do
      ok <- timeout (secs (roReadyTimeout opts)) (atomically (readTVar ready >>= check))
      a <- readTVarIO active
      unless (isJust ok && a) $ throwIO (RecorderError "kavach-recorder did not become ready")
  case startup of
    Right () -> pure ()
    Left e
      | roRequired opts -> do
          atomically (writeTVar active False)
          mapM_ terminateProcess proc'
          throwIO $ case fromException e of
            Just re@(RecorderError _) -> re
            Nothing -> RecorderError ("kavach-recorder could not be started: " ++ displayException e)
      | otherwise -> failRec r ("could not start the recorder: " ++ displayException (e :: SomeException))
  pure r

secs :: Double -> Int
secs = round . (* 1000000)

spawn :: [String] -> IO (Handle, Handle, ProcessHandle)
spawn [] = throwIO (RecorderError "empty recorder command")
spawn (c : args) = do
  -- the environment is left unchanged on purpose (SPEC §10.1)
  (Just i, Just o, _, p) <- createProcess (proc c args) {std_in = CreatePipe, std_out = CreatePipe}
  hSetBinaryMode i True
  hSetBinaryMode o True
  hSetBuffering i (BlockBuffering Nothing)
  pure (i, o, p)

openObject :: RecorderOptions -> Bool -> Maybe Ring -> Json
openObject o snaps ring =
  JObj $
    [ ("protocol", JNum "1")
    , ("service", JStr (roService o))
    , ("start", JStr (if roStart o == Genesis then "genesis" else "snapshot"))
    , ("producer", JStr (T.pack producer))
    , ("snapshots", JBool snaps)
    ]
      ++ concat [[("ring", JNum (show (ringCap g))), ("ring_path", JStr (T.pack (ringPath g)))] | Just g <- [ring]]
      ++ [("handler", JStr v) | Just v <- [roHandlerId o]]
      ++ [("dir", JStr v) | Just v <- [roDir o]]
      ++ [("compression", JStr v) | Just v <- [roCompression o]]
      ++ [(k, JNum (show v)) | (k, Just v) <- [("level", roLevel o), ("block_bytes", roBlockBytes o), ("flush_ms", roFlushMs o), ("segment_bytes", roSegmentBytes o), ("segment_seconds", roSegmentSeconds o), ("retain_segments", roRetainSegments o)]]
      ++ [("secret_keys", JArr (map JStr (roSecretKeys o))) | not (null (roSecretKeys o))]

controlLoop :: Recorder s -> Handle -> IO ()
controlLoop r out = do
  let loop = do
        l <- try (BC.hGetLine out)
        case l of
          Left e
            | isEOFError e -> pure ()
            | otherwise -> logLine ("control stream failed: " ++ show e)
          Right line -> do
            case parseJson line of
              Right msg | Just t <- field "t" msg >>= asText -> onControl r t msg
              _ -> logLine ("unreadable message from the recorder: " ++ show (BS.take 200 line))
            loop
  loop
  closing <- readTVarIO (rClosing r)
  unless closing $ failRec r "the recorder exited unexpectedly"
  atomically (writeTVar (rAck r) True >> writeTVar (rReady r) True)

onControl :: Recorder s -> Text -> Json -> IO ()
onControl r t msg = case T.unpack t of
  "ready" -> atomically (writeTVar (rReady r) True)
  "snapshot_request" -> atomically (writeTVar (rSnapReq r) True)
  "durable" -> atomically (modifyTVar' (rDurable r) (+ 1))
  "fixture" -> do
    let g k = maybe "" T.unpack (field k msg >>= asText)
        seqS = maybe "" (\v -> maybe (show v) T.unpack (asText v)) (field "seq" msg)
    logLine ("wrote fixture " ++ g "file" ++ " (input seq " ++ seqS ++ ", " ++ g "failure" ++ ")")
    roOnFixture (rOpts r) msg `catch` \e -> logLine ("onFixture callback failed: " ++ show (e :: SomeException))
  "error" -> do
    let fatal = (field "fatal" msg >>= asBool) == Just True
        m = maybe "" T.unpack (field "message" msg >>= asText)
    logLine ("recorder " ++ (if fatal then "fatal error: " else "error: ") ++ m)
    when fatal $ failRec r ("the recorder reported a fatal error: " ++ m)
  "closed" -> atomically (writeTVar (rAck r) True)
  _ -> pure ()

-- | Run the handler on one input, recording the step. A handler failure is
-- reported in the result, never thrown; only an asynchronous exception (after
-- the step has been recorded as a panic) escapes.
step :: Typeable s => Recorder s -> Input -> IO StepResult
step r inp = withMVar (rLock r) $ \_ -> do
  closed <- readIORef (rClosed r)
  when closed $ ioError (userError "kavach: step on a closed Recorder")
  answerSnapshotRequest r
  buf <- newIORef mempty
  outs <- newIORef []
  ckpt <- newIORef Nothing
  let h = rHandler r
      o = rOpts r
      rec' f = modifyIORef' buf (<> f)
      env =
        EnvImpl
          { envClock = do ns <- roClock o; rec' (recClock ns); pure ns
          , envRandom = \n -> do d <- roRandom o n; rec' (recRand d); pure d
          , envQuery = \g req -> do
              gw <- maybe (throwIO (ErrorCall ("kavach: gateway " ++ show g ++ " is not registered"))) pure (roGateways o g)
              res <- try (gwConnection gw req)
              let (resp, err) = case res of
                    Right b -> (b, "")
                    Left e -> (BS.empty, gatewayErrorText e)
              rec' (recGateway g req resp err (gwScope gw == Local))
              pure (if T.null err then Right resp else Left (GatewayError err))
          , envConfig = \k -> do v <- roConfig o k; rec' (recConfig k v (roConfigSource o)); pure v
          , envEmit = \sc sink d -> do
              modifyIORef' outs (Output sink d sc :)
              rec' (recOutput sink d (sc == Local))
          , envCommit = \d -> writeIORef ckpt (fromDynamic d)
          }
  s0 <- readIORef (rState r)
  -- The input goes in before the handler runs, so that a step that kills the
  -- process still leaves it on record (§10.2).
  rawWriteEnc r (recInput (inputSource inp) (inputPosition inp) (inputData inp))
  res <- runStep h env inp s0
  (failure, exc) <- case res of
    Right s' -> do
      writeIORef (rState r) s'
      f <- checkInvariants h s'
      pure (f, Nothing)
    Left (f, e) -> do
      kept <- readIORef ckpt
      writeIORef (rState r) (fromMaybe s0 kept)
      pure (Just f, Just e)
  case failure of
    Just f -> rec' (recMarker (failKind f) (failMessage f) (TE.encodeUtf8 (failDetail f)))
    Nothing -> pure ()
  rec' (encRaw frameStepEnd)
  frames <- readIORef buf
  rawWriteEnc r frames
  outputs <- reverse <$> readIORef outs
  when (failure == Nothing) $ roDeliver o outputs
  case exc >>= fromException of
    Just (SomeAsyncException _) -> maybe (pure ()) throwIO exc
    Nothing -> pure ()
  pure (StepResult failure exc outputs)

gatewayErrorText :: SomeException -> Text
gatewayErrorText e = case fromException e of
  Just (GatewayError t) -> t
  Nothing -> let m = T.pack (displayException e) in if T.null m then "error" else m

answerSnapshotRequest :: Recorder s -> IO ()
answerSnapshotRequest r = do
  pending <- readTVarIO (rSnapReq r)
  wanted <- if pending then atomically (swapTVar (rSnapReq r) False) else pure False
  active <- readTVarIO (rActive r)
  when (wanted && active && rSnapshots r) $ case snapshot (rHandler r) of
    Nothing -> pure ()
    Just f -> do
      s <- readIORef (rState r)
      res <- try (evaluate (f s))
      case res of
        Left e -> logLine ("snapshot failed; staying in the current segment: " ++ displayException (e :: SomeException))
        Right d -> rawWrite r (frameSnapshot d)

-- | Ask the recorder to close its open block now. With @durable@, wait (up to
-- ten seconds) until it is on disk. Returns False if recording has stopped or
-- the wait timed out. Call it between steps.
flush :: Recorder s -> Bool -> IO Bool
flush r durable = do
  before <- withMVar (rLock r) $ \_ -> do
    active <- readTVarIO (rActive r)
    if not active
      then pure Nothing
      else do
        b <- readTVarIO (rDurable r)
        rawWrite r (frameFlush durable)
        bell r
        pure (Just b)
  case before of
    Nothing -> pure False
    Just b
      | not durable -> readTVarIO (rActive r)
      | otherwise -> do
          _ <- timeout (secs 10) (atomically $ do
                 d <- readTVar (rDurable r)
                 a <- readTVar (rActive r)
                 check (d > b || not a))
          (> b) <$> readTVarIO (rDurable r)

-- | Orderly shutdown: send @close@ and wait for @closed@. Safe to call twice.
closeRecorder :: Recorder s -> IO ()
closeRecorder r = withMVar (rLock r) $ \_ -> do
  already <- atomicModifyIORef' (rClosed r) (\c -> (True, c))
  unless already $ (`finally` mapM_ destroyRing (rRing r)) $ case (rIn r, rProc r) of
    (Just i, Just p) -> do
      active <- readTVarIO (rActive r)
      when active $ do
        atomically (writeTVar (rClosing r) True)
        rawWrite r frameClose
        bell r
        ok <- timeout (secs (roCloseTimeout (rOpts r))) (atomically (readTVar (rAck r) >>= check))
        when (ok == Nothing) $ logLine "the recorder did not answer close in time"
      atomically (writeTVar (rClosing r) True >> writeTVar (rActive r) False)
      void (try (hClose i) :: IO (Either IOException ()))
      done <- timeout (secs 2) (waitForProcess p)
      when (done == Nothing) $ logLine "the recorder is still running after close"
    _ -> pure ()

-- | Run an action with a recorder, closing it afterwards.
withRecorder :: Typeable s => RecorderOptions -> Handler s -> (Recorder s -> IO a) -> IO a
withRecorder opts h = bracket (newRecorder opts h) closeRecorder
