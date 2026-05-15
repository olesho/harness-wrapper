"""Print the first 50 Fibonacci numbers."""


def fibonacci_numbers(count: int) -> list[int]:
    """Return the first ``count`` Fibonacci numbers starting with 0 and 1."""
    numbers: list[int] = []
    current: int = 0
    next_value: int = 1

    for _ in range(count):
        numbers.append(current)
        current, next_value = next_value, current + next_value

    return numbers


def main() -> None:
    """Print the first 50 Fibonacci numbers, one per line."""
    for number in fibonacci_numbers(50):
        print(number)


if __name__ == "__main__":
    main()
