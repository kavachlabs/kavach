module Main (main) where

import Control.Exception hiding (Handler, handle)
import Control.Monad (forM, forM_, unless)
import qualified Data.ByteString as BS
import Data.IORef
import Data.List (isInfixOf, isSuffixOf, sort)
import qualified Data.Text as T
import Kavach
import Kavach.Json
import Kavach.Wire
import RecorderCase (runCase, specDir)
import System.Directory (createDirectoryIfMissing, doesFileExist, getTemporaryDirectory, listDirectory, removeDirectoryRecursive)
import System.Environment (getEnvironment)
import System.Exit
import System.Posix.Process (getProcessID)
import System.Process
import Control.Concurrent (threadDelay)
import System.Timeout (timeout)
import GHC.Clock (getMonotonicTime)

check :: IORef Int -> String -> Bool -> IO ()
check failures name ok = do
  putStrLn ((if ok then "PASS  " else "FAIL  ") ++ name)
  unless ok $ modifyIORef' failures (+ 1)

data Shape = A | B deriving (Show)

partial :: Shape -> Int
partial A = 1

messageOf :: a -> IO T.Text
messageOf x = do
  r <- try (evaluate x)
  case r of
    Left e -> pure (failureMessage e)
    Right _ -> pure "no exception"

main :: IO ()
main = do
  failures <- newIORef 0
  let t = check failures

  t "base64 round trip" $
    and [base64Decode (base64Encode (BS.pack (take n [0 ..]))) == Just (BS.pack (take n [0 ..])) | n <- [0 .. 12]]
  t "base64 known values" $
    map base64Encode ["", "f", "fo", "foo", "foob"] == ["", "Zg==", "Zm8=", "Zm9v", "Zm9vYg=="]
  t "base64 rejects garbage" $
    base64Decode "Zm9" == Nothing && base64Decode "Zg=a" == Nothing && base64Decode "Z===" == Nothing

  t "json round trip" $
    fmap renderJson (parseJson "{ \"a\" : [1, true, null, \"x\\u00e9\\n\\ud83d\\ude00\"] }")
      == Right "{\"a\":[1,true,null,\"x\\u00e9\\n\\ud83d\\ude00\"]}"
  t "json rejects trailing junk" $ either (const True) (const False) (parseJson "{} x")

  t "uvarint framing: step_end is len 1, kind 3" $ frameStepEnd == BS.pack [1, 3]
  t "flush frame" $ frameFlush True == BS.pack [2, 6, 1]

  -- SPEC §4.5: the message carries no file paths, line numbers or call stacks.
  m1 <- messageOf (error "boom" :: Int)
  t ("error message is just the text: " ++ show m1) (m1 == "boom")
  m2 <- messageOf (head ([] :: [Int]))
  t ("head [] has no location: " ++ show m2) (not (any (`isInfixOf` T.unpack m2) [".hs", "CallStack", "called at"]) && not (T.null m2))
  m3 <- messageOf (partial B)
  t ("pattern-match failure has no span: " ++ show m3) ("Non-exhaustive" `isInfixOf` T.unpack m3 && not (".hs" `isInfixOf` T.unpack m3))
  m4 <- messageOf (1 `div` (0 :: Int))
  t "arithmetic exception" (m4 == "divide by zero")

  spec <- specDir
  let dir = spec ++ "/recorder/sdk"
  cases <- sort . filter ((== "json") . reverse . take 4 . reverse) <$> listDirectory dir
  rs <- forM [(noRing, c) | noRing <- [False, True], c <- cases] $ \(noRing, c) -> do
    r <- runCase noRing spec (dir ++ "/" ++ c)
    t ("recorder case " ++ c ++ (if noRing then " (pipe)" else " (ring)") ++ maybe "" (": " ++) r) (r == Nothing)
    pure r
  t "recorder cases found" (length cases == 7 && length rs == 14)

  killedService t

  hasRun <- doesFileExist (spec ++ "/host/run.py")
  ok <- if hasRun then (== ExitSuccess) <$> rawSystem "python3" [spec ++ "/host/run.py", "--host", "kavach-conformance-host"] else pure False
  t "host transcripts (run.py)" ok

  n <- readIORef failures
  putStrLn (show n ++ " failures")
  if n == 0 then exitSuccess else exitFailure

-- | A service killed right after publishing a step's input still leaves a
-- crash fixture (SPEC §10.7), over the ring and over the pipe, with the real
-- recorder.
killedService :: (String -> Bool -> IO ()) -> IO ()
killedService t = do
  tmp <- getTemporaryDirectory
  pid <- getProcessID
  let work = tmp ++ "/kavach-hs-kill-" ++ show pid
      goBuild out pkg = readCreateProcessWithExitCode (proc "go" ["build", "-o", work ++ "/" ++ out, pkg]) {cwd = Just "../.."} ""
  createDirectoryIfMissing True work
  (c1, _, e1) <- goBuild "kavach-recorder" "./cmd/kavach-recorder"
  (c2, _, e2) <- goBuild "kavach" "./cmd/kavach"
  t ("go build of the recorder and CLI" ++ e1 ++ e2) (c1 == ExitSuccess && c2 == ExitSuccess)
  env <- getEnvironment
  forM_ [("ring", []), ("pipe", ["--pipe"])] $ \(name, flags) -> do
    let dir = work ++ "/" ++ name
    createDirectoryIfMissing True dir
    (_, _, _, ph) <- createProcess (proc "kavach-kill-helper" (dir : flags)) {env = Just (("KAVACH_RECORDER", work ++ "/kavach-recorder") : env)}
    code <- waitForProcess ph
    t ("killed service over the " ++ name ++ " dies of SIGKILL") (code == ExitFailure (-9))
    -- the recorder is no child of this process: it finishes on its own
    let wait :: Int -> IO [FilePath]
        wait k = do
          fs <- map ((dir ++ "/fixtures/") ++) . filter (".kavach" `isSuffixOf`) <$> (listDirectory (dir ++ "/fixtures") `catch` \e -> const (pure []) (e :: IOException))
          if null fs && k > 0 then threadDelay 20000 >> wait (k - 1) else pure fs
    fs <- wait 500
    case fs of
      [f] -> do
        (_, out, _) <- readProcessWithExitCode (work ++ "/kavach") ["inspect", "--json", f] ""
        t ("killed service over the " ++ name ++ " leaves a crash fixture") ("crash" `isInfixOf` out)
      _ -> t ("killed service over the " ++ name ++ " leaves a fixture: " ++ show fs) False
  smallRing t work
  removeDirectoryRecursive work

sinkHandler :: Handler ()
sinkHandler =
  Handler
    { handle = \inp () -> if inputData inp == "boom" then kavachPanic "boom" else emit "out" (inputData inp)
    , initial = ()
    , snapshot = Nothing
    , restore = Nothing
    , invariants = []
    }

-- | Frames larger than the ring, and a ring that keeps filling, lose nothing;
-- a recorder that is gone ends the wait for space (§10.1).
smallRing :: (String -> Bool -> IO ()) -> FilePath -> IO ()
smallRing t work = do
  let dir = work ++ "/small"
      big = BS.replicate (200 * 1024) 98
      opts = (defaultOptions "small") {roDir = Just (T.pack dir), roRingBytes = Just (64 * 1024), roRecorderCommand = Just [work ++ "/kavach-recorder"]}
  r <- newRecorder opts sinkHandler
  forM_ [0 .. 399 :: Int] $ \i -> step r (Input "t" "p" (if i `mod` 100 == 7 then big else "small"))
  _ <- step r (Input "t" "p" "boom")
  closeRecorder r
  fs <- map ((dir ++ "/fixtures/") ++) . filter (".kavach" `isSuffixOf`) <$> listDirectory (dir ++ "/fixtures")
  case fs of
    [f] -> do
      (_, out, _) <- readProcessWithExitCode (work ++ "/kavach") ["inspect", "--json", f] ""
      t "small ring loses no input" (length (filter ("\"type\": \"input\"" `isInfixOf`) (lines out)) == 401)
    _ -> t ("small ring leaves a fixture: " ++ show fs) False
  r2 <- newRecorder (opts {roRecorderCommand = Just ["sleep", "1"]}) sinkHandler
  done <- timeout 15000000 (forM_ [1 .. 20 :: Int] $ \_ -> step r2 (Input "t" "p" (BS.replicate (30 * 1024) 120)))
  t "a recorder that is gone ends the wait for ring space" (done == Just ())
  closeRecorder r2
  -- A recorder that never says ready delays construction once, and steps after
  -- that do not wait (§10.1).
  t0 <- getMonotonicTime
  r3 <- newRecorder (opts {roRecorderCommand = Just ["sleep", "4"]}) sinkHandler
  t1 <- getMonotonicTime
  t "a silent recorder delays construction by the startup bound" (t1 - t0 > 1 && t1 - t0 < 3)
  forM_ [1 .. 100 :: Int] $ \_ -> step r3 (Input "t" "p" "x")
  t2 <- getMonotonicTime
  t "steps after a silent start do not wait" (t2 - t1 < 0.5)
  closeRecorder r3
