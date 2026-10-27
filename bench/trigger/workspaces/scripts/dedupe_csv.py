import csv
import sys


def dedupe(rows, key):
    seen = set()
    out = []
    for row in rows:
        if row[key] in seen:
            continue
        seen.add(row[key])
        out.append(row)
    return out


if __name__ == "__main__":
    reader = csv.DictReader(open(sys.argv[1]))
    rows = dedupe(list(reader), sys.argv[2])
    w = csv.DictWriter(sys.stdout, fieldnames=reader.fieldnames)
    w.writeheader()
    w.writerows(rows)
