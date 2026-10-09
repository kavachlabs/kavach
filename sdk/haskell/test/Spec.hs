module Main (main) where

import Control.Exception
import Control.Monad (forM, unless)
import qualified Data.ByteString as BS
import Data.IORef
import Data.List (isInfixOf, sort)
import qualified Data.Text as T
import Kavach
import Kavach.Json
import Kavach.Wire
import RecorderCase (runCase, specDir)
import System.Directory (doesFileExist, listDirectory)
import System.Exit
import System.Process (rawSystem)

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
  rs <- forM cases $ \c -> do
    r <- runCase spec (dir ++ "/" ++ c)
    t ("recorder case " ++ c ++ maybe "" (": " ++) r) (r == Nothing)
    pure r
  t "recorder cases found" (length cases == 7 && length rs == 7)

  hasRun <- doesFileExist (spec ++ "/host/run.py")
  ok <- if hasRun then (== ExitSuccess) <$> rawSystem "python3" [spec ++ "/host/run.py", "--host", "kavach-conformance-host"] else pure False
  t "host transcripts (run.py)" ok

  n <- readIORef failures
  putStrLn (show n ++ " failures")
  if n == 0 then exitSuccess else exitFailure
