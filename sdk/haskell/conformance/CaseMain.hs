-- | Runs one SDK recorder case (spec/recorder/sdk/README.md) through this SDK.
module Main (main) where

import RecorderCase (runCase, specDir)
import System.Environment (getArgs)
import System.Exit (exitFailure, exitSuccess)
import System.IO

main :: IO ()
main = do
  args <- getArgs
  case args of
    [path] -> run False path
    ["--pipe", path] -> run True path
    _ -> hPutStrLn stderr "usage: kavach-recorder-case [--pipe] <case.json>" >> exitFailure

run :: Bool -> FilePath -> IO ()
run noRing path = do
  dir <- specDir
  r <- runCase noRing dir path
  case r of
    Nothing -> putStrLn ("PASS  " ++ path) >> exitSuccess
    Just err -> putStrLn ("FAIL  " ++ path) >> hPutStrLn stderr err >> exitFailure
