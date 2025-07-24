import * as E from 'fp-ts/Either';
import * as J from 'fp-ts/Json';
import { pipe } from 'fp-ts/function';

export function serializeBody(body: unknown): E.Either<Error, string | undefined> {
  if (typeof body === 'string') {
    return E.right(body);
  }

  if (body) return pipe(J.stringify(body), E.mapLeft(E.toError));

  return E.right(undefined);
}
