from datetime import datetime


def parse(s):
    for fmt in ("%Y-%m-%d", "%d/%m/%Y", "%b %d %Y"):
        try:
            return datetime.strptime(s, fmt).date()
        except ValueError:
            pass
    raise ValueError(f"unrecognised date: {s}")
